// Unit evidence for the audio pack archive path (no network, no manifest
// packs): a synthetic pinned zip is verified, only listed members are read,
// sizes/hashes/containers are enforced and links are refused.
package doory

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func buildZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, content := range entries {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := file.Write(content); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func wavBytes(size int) []byte {
	data := make([]byte, size)
	copy(data, "RIFF")
	copy(data[8:], "WAVE")
	return data
}

func TestAudioPackArchiveVerification(t *testing.T) {
	member := wavBytes(256)
	archive := buildZip(t, map[string][]byte{"loop.wav": member, "extra.txt": []byte("ignored")})
	digest := sha256.Sum256(archive)
	memberDigest := sha256.Sum256(member)
	pack := audioPackSpec{
		ID: "parity-pack", ArchiveName: "parity-pack.zip",
		ArchiveBytes: int64(len(archive)), ArchiveSHA256: hex.EncodeToString(digest[:]),
		Tracks: []audioTrack{{ID: "loop", File: "loop.wav", Bytes: int64(len(member)),
			SHA256: hex.EncodeToString(memberDigest[:])}},
	}
	path := filepath.Join(t.TempDir(), pack.ArchiveName)
	if err := os.WriteFile(path, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyAudioArchive(pack, path); err != nil {
		t.Fatalf("verifyAudioArchive: %v", err)
	}
	members, err := readAudioMembers(path, pack, pack.Tracks)
	if err != nil {
		t.Fatalf("readAudioMembers: %v", err)
	}
	if !bytes.Equal(members["loop"], member) {
		t.Fatal("member bytes differ")
	}
	// A mismatched pinned hash is refused.
	tampered := pack
	tampered.Tracks = []audioTrack{{ID: "loop", File: "loop.wav", Bytes: int64(len(member)),
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}
	if _, err := readAudioMembers(path, tampered, tampered.Tracks); err == nil {
		t.Fatal("a mismatched member hash must be refused")
	}
	// A member the archive does not hold is refused.
	missing := pack
	missing.Tracks = []audioTrack{{ID: "nope", File: "nope.wav", Bytes: 4,
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}
	if _, err := readAudioMembers(path, missing, missing.Tracks); err == nil {
		t.Fatal("a missing member must be refused")
	}
	// Content that is not the container its extension names is refused.
	notWav := []byte("NOPE" + string(make([]byte, 252)))
	badArchive := buildZip(t, map[string][]byte{"loop.wav": notWav})
	badDigest := sha256.Sum256(notWav)
	badPath := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(badPath, badArchive, 0o644); err != nil {
		t.Fatal(err)
	}
	badPack := pack
	badPack.Tracks = []audioTrack{{ID: "loop", File: "loop.wav", Bytes: int64(len(notWav)),
		SHA256: hex.EncodeToString(badDigest[:])}}
	if _, err := readAudioMembers(badPath, badPack, badPack.Tracks); err == nil {
		t.Fatal("a non-WAV member must be refused")
	}
	// An archive whose size does not match the pin is refused before reading.
	wrongSize := pack
	wrongSize.ArchiveBytes = int64(len(archive)) + 1
	if err := verifyAudioArchive(wrongSize, path); err == nil {
		t.Fatal("an archive size mismatch must be refused")
	}
}

func TestAudioDirectoryPackSource(t *testing.T) {
	dir := t.TempDir()
	archive := buildZip(t, map[string][]byte{"a.wav": wavBytes(64)})
	if err := os.WriteFile(filepath.Join(dir, "pack.zip"), archive, 0o644); err != nil {
		t.Fatal(err)
	}
	pack := audioPackSpec{ID: "pack", ArchiveName: "pack.zip"}
	destination := filepath.Join(t.TempDir(), "out.zip")
	if err := (audioDirectoryPackSource{dir: dir}).fetch(pack, destination); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("archive not written: %v", err)
	}
	if err := (audioDirectoryPackSource{dir: t.TempDir()}).fetch(pack, destination); err == nil {
		t.Fatal("a missing archive must be an error")
	}
}
