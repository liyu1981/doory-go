// Turning a checked clip into a stored asset: right away when rewrapping is
// enough, otherwise in a background goroutine with status=processing.
package doory

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	videoStaleGraceSeconds = 5 * 60
	videoInterrupted       = "The conversion was interrupted. Upload the video again."
	videoFailedMessage     = "The video could not be converted."
	videoQuotaMessage      = "Your storage quota is full. Delete some uploads to free up space."
	videoTooManyMessage    = "You have several videos converting already. Wait for one to finish, then try again."
)

type videoConverted struct {
	clip   string
	info   videoInfo
	stills videoStills
	audio  string
}

func (a *App) videoStoredSize(converted videoConverted) int64 {
	size := fileSize(converted.clip)
	size += int64(len(converted.stills.Poster)) + int64(len(converted.stills.Thumbnail))
	size += int64(len(converted.stills.Filmstrip))
	if converted.audio != "" {
		size += fileSize(converted.audio)
	}
	return size
}

func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (a *App) videoAssetPaths(ownerID, id string) (file, poster, thumb, strip, audio string) {
	base := "assets/" + dashedUUID(ownerID) + "/"
	return base + dashedUUID(id) + ".mp4",
		base + "posters/" + dashedUUID(id) + ".webp",
		base + "thumbs/" + dashedUUID(id) + ".webp",
		base + "strips/" + dashedUUID(id) + ".webp",
		base + "audio/" + dashedUUID(id) + ".m4a"
}

func (a *App) convertVideo(source string, info videoInfo) (videoConverted, error) {
	workdir := filepath.Dir(source)
	target := filepath.Join(workdir, "clip.mp4")
	result, err := a.normalizeVideo(source, target, info)
	if err != nil {
		return videoConverted{}, err
	}
	stills, err := a.makeVideoStills(target, result)
	if err != nil {
		return videoConverted{}, err
	}
	audioPath := filepath.Join(workdir, "audio.m4a")
	hasAudio := result.HasAudio && a.extractVideoAudio(target, audioPath)
	converted := videoConverted{clip: target, info: result, stills: stills}
	if hasAudio {
		converted.audio = audioPath
	}
	return converted, nil
}

// ingestVideoUpload stores an uploaded clip, or returns (nil,false,nil) when
// the file has no video (the caller falls back to the audio/raster pipeline).
func (a *App) ingestVideoUpload(owner *UserRow, data []byte, name string, tags []string) (*assetRow, bool, *DocumentError) {
	if !a.videoAvailable() || !videoLooksLikeMedia(data) {
		return nil, false, nil
	}
	workdir, err := a.videoWorkDir(videoWorkPrefix)
	if err != nil {
		return nil, true, &DocumentError{Code: "server_error", Status: 500,
			Message: "Something went wrong on our side."}
	}
	source := filepath.Join(workdir, "source")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		_ = os.RemoveAll(workdir)
		return nil, true, &DocumentError{Code: "server_error", Status: 500,
			Message: "Something went wrong on our side."}
	}
	info, perr := a.probeVideo(source)
	if perr != nil {
		_ = os.RemoveAll(workdir)
		var ve *videoError
		if AsVideoError(perr, &ve) && ve.notAVideo && looksLikeAudio(data) {
			return nil, false, nil
		}
		return nil, true, &DocumentError{Code: "invalid_file", Status: 400, Message: perr.Error()}
	}
	asset, created, derr := a.createVideoAsset(owner, source, info, name, tags, nil, nil)
	_ = created
	if derr != nil {
		_ = os.RemoveAll(workdir)
	}
	return asset, true, derr
}

func AsVideoError(err error, target **videoError) bool {
	for err != nil {
		if ve, ok := err.(*videoError); ok {
			*target = ve
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// createVideoAsset registers the clip. `same` is the stock-provider duplicate
// key (meta provider + id); a matching non-failed clip is returned instead.
func (a *App) createVideoAsset(owner *UserRow, source string, info videoInfo, name string,
	tags []string, meta map[string]any, same map[string]string,
) (*assetRow, bool, *DocumentError) {
	if same != nil {
		existing, derr := a.findVideoBySame(owner, same)
		if derr != nil {
			return nil, false, derr
		}
		if existing != nil {
			return existing, false, nil
		}
	}
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	videoMeta := map[string]any{}
	for key, value := range meta {
		videoMeta[key] = value
	}
	videoMeta["video"] = info.asMeta()
	metaText := string(encodeCompactJSON(videoMeta))
	tagsText := mustMarshalString(tags)
	file, poster, thumb, strip, audio := a.videoAssetPaths(owner.ID, id)

	if !info.webReady() {
		// Reserve a processing row and convert in the background.
		if derr := a.reserveVideoRow(owner, id, name, info, tagsText, metaText, now); derr != nil {
			return nil, false, derr
		}
		a.submitVideoJob(id, source, info)
		row, err := a.getAssetByID(id)
		if err != nil || row == nil {
			return nil, false, &DocumentError{Code: "server_error", Status: 500,
				Message: "Something went wrong on our side."}
		}
		return row, true, nil
	}

	release, berr := a.videoSlot(videoSlotWaitSeconds * time.Second)
	if berr != nil {
		return nil, false, &DocumentError{Code: "server_busy", Status: 503,
			Message: "The server is busy processing other large uploads. Try again in a moment."}
	}
	converted, cerr := a.convertVideo(source, info)
	release()
	if cerr != nil {
		return nil, false, &DocumentError{Code: "invalid_file", Status: 400, Message: cerr.Error()}
	}
	storedSize := a.videoStoredSize(converted)
	used, _ := a.storageUsed(owner.ID)
	if used+storedSize > a.Settings.UserStorageQuotaBytes {
		return nil, false, &DocumentError{Code: "quota_exceeded", Status: 413, Message: videoQuotaMessage}
	}
	asset := &assetRow{
		ID: id, OwnerID: owner.ID, Kind: "video", Name: name, Width: int64(converted.info.Width),
		Height: int64(converted.info.Height), Size: storedSize, Mime: videoMP4MIME,
		Tags: tagsText, Meta: videoMetaTextFrom(metaText, converted), File: file, Poster: poster,
		Thumbnail: thumb, Filmstrip: strip, Audio: audio, Status: "ready", CreatedAt: now, UpdatedAt: now,
	}
	if converted.stills.Filmstrip == nil {
		asset.Filmstrip = ""
	}
	if converted.audio == "" {
		asset.Audio = ""
	}
	if err := a.storeVideoFiles(asset, converted); err != nil {
		return nil, false, &DocumentError{Code: "server_error", Status: 500,
			Message: "Something went wrong on our side."}
	}
	if _, err := a.DB.Exec(
		"INSERT INTO assets_asset ("+assetCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		asset.ID, asset.OwnerID, asset.Kind, asset.Name, asset.Width, asset.Height, asset.Size,
		asset.Mime, asset.Tags, asset.Meta, asset.File, asset.Thumbnail, asset.Poster,
		asset.Filmstrip, asset.Audio, asset.Status, asset.Error, now, now); err != nil {
		a.deleteVideoFiles(asset)
		return nil, false, &DocumentError{Code: "server_error", Status: 500,
			Message: "Something went wrong on our side."}
	}
	_ = os.RemoveAll(filepath.Dir(source))
	row, err := a.getAssetByID(id)
	if err != nil || row == nil {
		return nil, false, &DocumentError{Code: "server_error", Status: 500,
			Message: "Something went wrong on our side."}
	}
	return row, true, nil
}

func videoMetaTextFrom(base string, converted videoConverted) string {
	meta := map[string]any{}
	_ = json.Unmarshal([]byte(base), &meta)
	info := converted.info.asMeta()
	if converted.stills.Strip != nil {
		info["strip"] = converted.stills.Strip
	}
	meta["video"] = info
	return string(encodeCompactJSON(meta))
}

func (a *App) reserveVideoRow(owner *UserRow, id, name string, info videoInfo, tags, meta, now string) *DocumentError {
	a.failStaleVideoJobs("")
	if a.Settings.VideoMaxProcessingPerUser > 0 {
		var count int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM assets_asset WHERE owner_id = ? AND kind = 'video' AND status = 'processing'`,
			owner.ID).Scan(&count)
		if count >= a.Settings.VideoMaxProcessingPerUser {
			return &DocumentError{Code: "too_many_conversions", Status: 429, Message: videoTooManyMessage}
		}
	}
	used, _ := a.storageUsed(owner.ID)
	if used+int64(info.Size) > a.Settings.UserStorageQuotaBytes {
		return &DocumentError{Code: "quota_exceeded", Status: 413, Message: videoQuotaMessage}
	}
	if _, err := a.DB.Exec(
		"INSERT INTO assets_asset ("+assetCols+") VALUES (?, ?, 'video', ?, ?, ?, ?, ?, ?, ?, '', '', '', '', '', 'processing', '', ?, ?)",
		id, owner.ID, name, info.Width, info.Height, info.Size, videoMP4MIME, tags, meta, now, now); err != nil {
		return &DocumentError{Code: "server_error", Status: 500, Message: "Something went wrong on our side."}
	}
	return nil
}

func (a *App) storeVideoFiles(asset *assetRow, converted videoConverted) error {
	clip, err := os.ReadFile(converted.clip)
	if err != nil {
		return err
	}
	if err := adminWriteMedia(a.Settings.MediaRoot, asset.File, clip); err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
	}{
		{asset.Poster, converted.stills.Poster},
		{asset.Thumbnail, converted.stills.Thumbnail},
		{asset.Filmstrip, converted.stills.Filmstrip},
	}
	for _, file := range files {
		if file.name == "" || file.data == nil {
			continue
		}
		if err := adminWriteMedia(a.Settings.MediaRoot, file.name, file.data); err != nil {
			return err
		}
	}
	if converted.audio != "" {
		data, err := os.ReadFile(converted.audio)
		if err != nil {
			return err
		}
		if err := adminWriteMedia(a.Settings.MediaRoot, asset.Audio, data); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) deleteVideoFiles(asset *assetRow) {
	for _, name := range []string{asset.File, asset.Poster, asset.Thumbnail, asset.Filmstrip, asset.Audio} {
		a.deleteMediaFile(name)
	}
}

func (a *App) findVideoBySame(owner *UserRow, same map[string]string) (*assetRow, *DocumentError) {
	rows, err := a.DB.Query(`SELECT `+assetCols+` FROM assets_asset
		WHERE owner_id = ? AND kind = 'video' AND status <> 'failed' ORDER BY created_at DESC, id ASC`, owner.ID)
	if err != nil {
		return nil, &DocumentError{Code: "server_error", Status: 500, Message: "Something went wrong on our side."}
	}
	defer rows.Close()
	for rows.Next() {
		asset, scanErr := scanAsset(rows)
		if scanErr != nil {
			continue
		}
		var meta map[string]any
		if json.Unmarshal([]byte(asset.Meta), &meta) != nil {
			continue
		}
		match := true
		for key, value := range same {
			if stringFromAny(meta[key]) != value {
				match = false
				break
			}
		}
		if match {
			return &asset, nil
		}
	}
	return nil, nil
}

// ---- background conversions --------------------------------------------------------------------

var (
	videoJobsMu   sync.Mutex
	videoJobPool  chan struct{}
	videoJobPoolN int
)

// videoJobSlot leaves one ffmpeg slot free for web-ready rewraps: background
// conversions run on max(1, VIDEO_MAX_CONCURRENCY-1) workers.
func (a *App) videoJobSlot() func() {
	size := 1
	if a.Settings != nil && a.Settings.VideoMaxConcurrency > 1 {
		size = a.Settings.VideoMaxConcurrency - 1
	}
	videoJobsMu.Lock()
	if videoJobPool == nil || videoJobPoolN != size {
		videoJobPool = make(chan struct{}, size)
		videoJobPoolN = size
	}
	pool := videoJobPool
	videoJobsMu.Unlock()
	pool <- struct{}{}
	return func() { <-pool }
}

func (a *App) submitVideoJob(id, source string, info videoInfo) {
	go a.runVideoJob(id, source, info)
}

func (a *App) runVideoJob(id, source string, info videoInfo) {
	defer os.RemoveAll(filepath.Dir(source))
	releaseJob := a.videoJobSlot()
	defer releaseJob()
	release, err := a.videoSlot(-1)
	if err != nil {
		a.failVideoJob(id, videoFailedMessage)
		return
	}
	defer release()
	if !a.startVideoJob(id) {
		return
	}
	converted, err := a.convertVideo(source, info)
	if err != nil {
		a.failVideoJob(id, err.Error())
		return
	}
	var row assetRow
	dbRow := a.DB.QueryRow("SELECT "+assetCols+" FROM assets_asset WHERE id = ? AND status = 'processing'", id)
	row, err = scanAsset(dbRow)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		a.failVideoJob(id, videoFailedMessage)
		return
	}
	size := a.videoStoredSize(converted)
	used, _ := a.storageUsed(row.OwnerID)
	// exclude this row's held source size
	used -= row.Size
	if used+size > a.Settings.UserStorageQuotaBytes {
		a.failVideoJob(id, videoQuotaMessage)
		return
	}
	file, poster, thumb, strip, audio := a.videoAssetPaths(row.OwnerID, id)
	row.File, row.Poster, row.Thumbnail, row.Filmstrip, row.Audio = file, poster, thumb, strip, audio
	if converted.stills.Filmstrip == nil {
		row.Filmstrip = ""
	}
	if converted.audio == "" {
		row.Audio = ""
	}
	row.Width, row.Height, row.Size = int64(converted.info.Width), int64(converted.info.Height), size
	row.Meta = videoMetaTextFrom(row.Meta, converted)
	row.Mime, row.Status, row.Error = videoMP4MIME, "ready", ""
	if err := a.storeVideoFiles(&row, converted); err != nil {
		a.failVideoJob(id, videoFailedMessage)
		return
	}
	res, err := a.DB.Exec(`UPDATE assets_asset SET file = ?, poster = ?, thumbnail = ?, filmstrip = ?,
		audio = ?, width = ?, height = ?, size = ?, mime = ?, meta = ?, status = 'ready', error = '',
		updated_at = ? WHERE id = ? AND status = 'processing'`,
		row.File, row.Poster, row.Thumbnail, row.Filmstrip, row.Audio, row.Width, row.Height, row.Size,
		row.Mime, row.Meta, formatDjangoDateTime(time.Now()), id)
	if err != nil {
		a.failVideoJob(id, videoFailedMessage)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.deleteVideoFiles(&row)
	}
}

func (a *App) startVideoJob(id string) bool {
	res, err := a.DB.Exec(`UPDATE assets_asset SET updated_at = ? WHERE id = ? AND status = 'processing'`,
		formatDjangoDateTime(time.Now()), id)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

func (a *App) failVideoJob(id, message string) {
	if message == "" {
		message = videoFailedMessage
	}
	if len([]rune(message)) > 255 {
		message = string([]rune(message)[:255])
	}
	_, _ = a.DB.Exec(`UPDATE assets_asset SET status = 'failed', error = ?, size = 0, updated_at = ?
		WHERE id = ? AND status = 'processing'`, message, formatDjangoDateTime(time.Now()), id)
}

func (a *App) videoStaleAfterSeconds() int {
	return a.Settings.VideoTranscodeTimeoutSeconds + 2*int(videoStillsTimeout/time.Second) + videoStaleGraceSeconds
}

func (a *App) failStaleVideoJobs(ownerID string) int {
	cutoff := formatDjangoDateTime(time.Now().Add(-time.Duration(a.videoStaleAfterSeconds()) * time.Second))
	query := `UPDATE assets_asset SET status = 'failed', error = ?, size = 0, updated_at = ?
		WHERE kind = 'video' AND status = 'processing' AND updated_at < ?`
	args := []any{videoInterrupted, formatDjangoDateTime(time.Now()), cutoff}
	if ownerID != "" {
		query += " AND owner_id = ?"
		args = append(args, ownerID)
	}
	res, err := a.DB.Exec(query, args...)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		a.removeOldVideoWorkDirs(float64(a.videoStaleAfterSeconds()))
	}
	return int(n)
}

func (a *App) sweepVideoJobs() (int, int) {
	count := a.failStaleVideoJobs("")
	removed := a.removeOldVideoWorkDirs(float64(a.videoStaleAfterSeconds()))
	return count, removed
}
