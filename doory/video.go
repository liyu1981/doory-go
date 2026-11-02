// Video clips: probe an upload with ffprobe and make it web-ready with ffmpeg.
//
// The tools run as separate commands with a fixed argument list, a small
// environment, a private work directory and a deadline (video.py). Every clip
// becomes H.264/yuv420p + AAC MP4 with +faststart, at most 1920x1080, plus a
// poster, a thumbnail, a filmstrip and an .m4a of its sound.
package doory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	videoMP4MIME          = "video/mp4"
	videoProbeTimeout     = 20 * time.Second
	videoRemuxTimeout     = 60 * time.Second
	videoStillsTimeout    = 60 * time.Second
	videoMaxProbeOutput   = 256 * 1024
	videoStderrTail       = 2000
	videoMaxDimension     = 8192
	videoMaxWidth         = 1920
	videoMaxHeight        = 1080
	videoMaxFPS           = 60
	videoMaxSourceFPS     = 240
	videoVariableRateFPS  = 30
	videoPosterMaxSide    = 1920
	videoPosterQuality    = 88
	videoStripHeight      = 90
	videoStripMaxFrames   = 60
	videoStripMinInterval = 500
	videoSlotWaitSeconds  = 20.0
	videoWorkPrefix       = "doory-video-"
)

var videoContainers = map[string]bool{
	"mov": true, "mp4": true, "m4a": true, "3gp": true, "3g2": true, "mj2": true,
	"matroska": true, "webm": true, "avi": true,
}
var videoRecordingContainers = map[string]bool{"matroska": true, "webm": true}
var videoStillCodecs = map[string]bool{"gif": true, "png": true, "mjpeg": true, "apng": true,
	"webp": true, "bmp": true, "tiff": true}

type videoError struct {
	message   string
	notAVideo bool
}

func (e *videoError) Error() string { return e.message }

func videoErr(message string) error     { return &videoError{message: message} }
func notAVideoErr(message string) error { return &videoError{message: message, notAVideo: true} }

type videoInfo struct {
	DurationMS int
	Width      int
	Height     int
	FPS        float64
	Codec      string
	PixFmt     string
	Rotation   int
	Constant   bool
	HasAudio   bool
	AudioCodec string
	HDR        bool
	Size       int
}

func (v videoInfo) webReady() bool {
	longSide, shortSide := maxInt(v.Width, v.Height), v.Width
	if v.Height < shortSide {
		shortSide = v.Height
	}
	return v.DurationMS > 0 && v.Codec == "h264" &&
		(v.PixFmt == "yuv420p" || v.PixFmt == "yuvj420p") &&
		longSide <= videoMaxWidth && shortSide <= videoMaxHeight && v.Rotation == 0 &&
		v.Constant && v.FPS <= videoMaxFPS && !v.HDR &&
		(!v.HasAudio || v.AudioCodec == "aac")
}

func (v videoInfo) asMeta() map[string]any {
	return map[string]any{
		"duration_ms":  v.DurationMS,
		"width":        v.Width,
		"height":       v.Height,
		"fps":          math.Round(v.FPS*1000) / 1000,
		"has_audio":    v.HasAudio,
		"source_codec": v.Codec,
	}
}

type videoStills struct {
	Poster    []byte
	Thumbnail []byte
	Filmstrip []byte
	Strip     map[string]any
}

// ---- bounded concurrency -----------------------------------------------------------------------

var (
	videoSlotsMu sync.Mutex
	videoSlots   chan struct{}
	videoSlotsN  int
)

func (a *App) videoSlot(wait time.Duration) (func(), error) {
	size := 1
	if a.Settings != nil && a.Settings.VideoMaxConcurrency > 0 {
		size = a.Settings.VideoMaxConcurrency
	}
	videoSlotsMu.Lock()
	if videoSlots == nil || videoSlotsN != size {
		videoSlots = make(chan struct{}, size)
		videoSlotsN = size
	}
	slots := videoSlots
	videoSlotsMu.Unlock()
	if wait < 0 {
		slots <- struct{}{}
		return func() { <-slots }, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-timer.C:
		return nil, videoErr("The server is busy converting other videos. Try again in a moment.")
	}
}

// ---- work directories --------------------------------------------------------------------------

func (a *App) videoWorkDir(prefix string) (string, error) {
	root := ""
	if a.Settings != nil {
		root = a.Settings.VideoWorkDir
	}
	if root != "" {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", err
		}
	}
	return os.MkdirTemp(root, prefix)
}

func videoLastChange(path string) float64 {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	newest := float64(info.ModTime().UnixNano()) / 1e9
	entries, err := os.ReadDir(path)
	if err != nil {
		return newest
	}
	for _, entry := range entries {
		if child, err := entry.Info(); err == nil {
			if t := float64(child.ModTime().UnixNano()) / 1e9; t > newest {
				newest = t
			}
		}
	}
	return newest
}

func (a *App) removeOldVideoWorkDirs(maxAgeSeconds float64) int {
	root := ""
	if a.Settings != nil {
		root = a.Settings.VideoWorkDir
	}
	if root == "" {
		root = os.TempDir()
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	cutoff := float64(time.Now().UnixNano())/1e9 - maxAgeSeconds
	removed := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), videoWorkPrefix) || !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if videoLastChange(path) >= cutoff {
			continue
		}
		if err := os.RemoveAll(path); err == nil {
			removed++
		}
	}
	return removed
}

// ---- running the commands ----------------------------------------------------------------------

type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	write := len(p)
	if l.buf.Len()+len(p) > l.limit {
		l.overflow = true
		left := l.limit - l.buf.Len()
		if left > 0 {
			_, _ = l.buf.Write(p[:left])
		}
		return write, nil
	}
	_, _ = l.buf.Write(p)
	return write, nil
}

func (a *App) videoCommandEnv(workdir string) []string {
	return allowlistedEnv(workdir)
}

func (a *App) runVideoCommand(args []string, timeout time.Duration, workdir string, capture bool) ([]byte, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = a.videoCommandEnv(workdir)
	if workdir != "" {
		cmd.Dir = workdir
	}
	cmd.SysProcAttr = sysProcAttrSetsid()
	stdout := &limitedBuffer{limit: videoMaxProbeOutput}
	stderr := &limitedBuffer{limit: videoStderrTail}
	if capture {
		cmd.Stdout = stdout
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, videoErr("The video could not be read.")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return nil, videoErr("The video could not be read.")
		}
	case <-time.After(timeout):
		killProcessGroup(cmd)
		return nil, videoErr("Converting the video took too long.")
	}
	if capture && stdout.overflow {
		return nil, videoErr("The video could not be read.")
	}
	return stdout.buf.Bytes(), nil
}

func (a *App) ffmpegArgs(args ...string) []string {
	return append([]string{a.Settings.FFmpegPath, "-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...)
}

func videoInput(path string) []string {
	return []string{"-protocol_whitelist", "file", "-format_whitelist", "mov,matroska,avi", "-i", path}
}

// ---- probing -----------------------------------------------------------------------------------

func videoFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		if strings.Contains(x, "/") {
			parts := strings.SplitN(x, "/", 2)
			n, e1 := strconv.ParseFloat(parts[0], 64)
			d, e2 := strconv.ParseFloat(parts[1], 64)
			if e1 == nil && e2 == nil && d != 0 && n > 0 {
				return n / d
			}
			return 0
		}
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func videoInt(v any) int { return int(videoFloat(v)) }
func videoString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func videoMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
func videoList(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return nil
}

func readVideoProbe(data map[string]any, size int, maxDurationSeconds int) (videoInfo, error) {
	fmtMap := videoMap(data["format"])
	formats := map[string]bool{}
	for _, part := range strings.Split(videoString(fmtMap["format_name"]), ",") {
		formats[part] = true
	}
	okFormat := false
	for name := range formats {
		if videoContainers[name] {
			okFormat = true
		}
	}
	if !okFormat {
		return videoInfo{}, videoErr("This video format is not supported.")
	}
	streams := videoList(data["streams"])
	var videos []map[string]any
	var audios []map[string]any
	for _, raw := range streams {
		stream := videoMap(raw)
		switch videoString(stream["codec_type"]) {
		case "video":
			if videoInt(videoMap(stream["disposition"])["attached_pic"]) == 0 {
				videos = append(videos, stream)
			}
		case "audio":
			audios = append(audios, stream)
		}
	}
	if len(videos) == 0 {
		return videoInfo{}, notAVideoErr("The file has no video in it.")
	}
	video := videos[0]
	codec := videoString(video["codec_name"])
	if videoStillCodecs[codec] {
		return videoInfo{}, videoErr("The file is a picture, not a video.")
	}
	width, height := videoInt(video["width"]), videoInt(video["height"])
	if width <= 0 || height <= 0 || width > videoMaxDimension || height > videoMaxDimension {
		return videoInfo{}, videoErr("The video's size is not supported.")
	}
	duration := videoFloat(fmtMap["duration"])
	if duration <= 0 {
		for _, stream := range streams {
			if value := videoFloat(videoMap(stream)["duration"]); value > duration {
				duration = value
			}
		}
	}
	recording := false
	for name := range formats {
		if videoRecordingContainers[name] {
			recording = true
		}
	}
	if duration <= 0 && !recording {
		return videoInfo{}, videoErr("The video has no length.")
	}
	if duration > float64(maxDurationSeconds) {
		minutes := float64(maxDurationSeconds) / 60
		return videoInfo{}, videoErr(fmt.Sprintf("The video is too long: clips can be at most %g minutes.", minutes))
	}
	real, average := videoFloat(video["r_frame_rate"]), videoFloat(video["avg_frame_rate"])
	var sane []float64
	for _, rate := range []float64{average, real} {
		if rate > 0 && rate <= videoMaxSourceFPS {
			sane = append(sane, rate)
		}
	}
	fps := float64(videoVariableRateFPS)
	if len(sane) > 0 {
		fps = sane[0]
	}
	rotation := 0
	for _, rawSide := range videoList(video["side_data_list"]) {
		if angle, ok := videoMap(rawSide)["rotation"]; ok {
			rotation = int(math.Round(videoFloat(angle)/90)) * 90 % 360
		}
	}
	if rotation == 0 {
		rotation = int(math.Round(videoFloat(videoMap(video["tags"])["rotate"])/90)) * 90 % 360
	}
	if rotation == 90 || rotation == 270 {
		width, height = height, width
	}
	transfer := videoString(video["color_transfer"])
	constant := len(sane) > 0 && real > 0 && average > 0 && math.Abs(real-average)/real < 0.01
	info := videoInfo{
		DurationMS: int(math.Round(duration * 1000)), Width: width, Height: height, FPS: fps,
		Codec: codec, PixFmt: videoString(video["pix_fmt"]), Rotation: rotation,
		Constant: constant, HasAudio: len(audios) > 0, HDR: transfer == "smpte2084" || transfer == "arib-std-b67",
		Size: size,
	}
	if len(audios) > 0 {
		info.AudioCodec = videoString(audios[0]["codec_name"])
	}
	return info, nil
}

func (a *App) probeVideo(path string) (videoInfo, error) {
	out, err := a.runVideoCommand(append([]string{a.Settings.FFprobePath, "-v", "error",
		"-print_format", "json", "-show_format", "-show_streams"}, videoInput(path)...),
		videoProbeTimeout, "", true)
	if err != nil {
		return videoInfo{}, err
	}
	var data map[string]any
	if err := json.Unmarshal(out, &data); err != nil {
		return videoInfo{}, videoErr("The video could not be read.")
	}
	stat, err := os.Stat(path)
	if err != nil {
		return videoInfo{}, videoErr("The video could not be read.")
	}
	return readVideoProbe(data, int(stat.Size()), a.Settings.MaxVideoDurationSeconds)
}

// ---- web-ready output --------------------------------------------------------------------------

func even(value float64) int {
	v := int(value) / 2 * 2
	if v < 2 {
		return 2
	}
	return v
}

func videoOutputSize(info videoInfo) (int, int) {
	boxW, boxH := videoMaxWidth, videoMaxHeight
	if info.Width < info.Height {
		boxW, boxH = videoMaxHeight, videoMaxWidth
	}
	scale := math.Min(1, math.Min(float64(boxW)/float64(info.Width), float64(boxH)/float64(info.Height)))
	return even(float64(info.Width) * scale), even(float64(info.Height) * scale)
}

func (a *App) videoMaxOutputBytes() int {
	return 2 * a.Settings.MaxVideoUploadBytes
}

func (a *App) videoBounds() []string {
	threads := a.Settings.VideoFFmpegThreads
	if threads < 1 {
		threads = 1
	}
	cpus := runtimeCPUCount()
	return []string{"-t", strconv.Itoa(a.Settings.MaxVideoDurationSeconds),
		"-fs", strconv.Itoa(a.videoMaxOutputBytes()),
		"-threads", strconv.Itoa(threads), "-filter_threads", strconv.Itoa(threads),
		"-timelimit", strconv.Itoa(a.Settings.VideoTranscodeTimeoutSeconds * cpus)}
}

func runtimeCPUCount() int {
	n := 0
	if entries, err := os.ReadDir("/sys/devices/system/cpu"); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "cpu") && len(entry.Name()) > 3 {
				if _, err := strconv.Atoi(entry.Name()[3:]); err == nil {
					n++
				}
			}
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

func (a *App) videoRemuxArgs(source, target string) []string {
	return a.ffmpegArgs(append(videoInput(source),
		"-map", "0:v:0", "-map", "0:a:0?", "-c", "copy", "-map_metadata", "-1",
		"-movflags", "+faststart", target)...)
}

func (a *App) videoTranscodeArgs(source, target string, info videoInfo) []string {
	width, height := videoOutputSize(info)
	fps := math.Min(info.FPS, videoMaxFPS)
	keyframes := int(math.Round(fps * 2))
	if keyframes < 1 {
		keyframes = 1
	}
	filters := []string{fmt.Sprintf("scale=%d:%d:flags=lanczos", width, height), "format=yuv420p"}
	if info.HDR {
		filters = append([]string{"zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv"}, filters...)
	}
	if !info.Constant || info.FPS > videoMaxFPS {
		filters = append(filters, fmt.Sprintf("fps=%.3f", fps))
	}
	audio := []string{"-an"}
	if info.HasAudio {
		audio = []string{"-c:a", "aac", "-b:a", "160k", "-ac", "2", "-ar", "48000"}
	}
	args := append(videoInput(source), "-map", "0:v:0", "-map", "0:a:0?", "-vf", strings.Join(filters, ","),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "22", "-profile:v", "high",
		"-g", strconv.Itoa(keyframes), "-keyint_min", strconv.Itoa(maxInt(1, int(math.Round(fps)))),
		"-sc_threshold", "0", "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
	args = append(args, audio...)
	args = append(args, "-map_metadata", "-1", "-movflags", "+faststart")
	args = append(args, a.videoBounds()...)
	args = append(args, target)
	return a.ffmpegArgs(args...)
}

func (a *App) hasZscale() bool {
	out, err := a.runVideoCommand([]string{a.Settings.FFmpegPath, "-hide_banner", "-filters"}, 10*time.Second, "", true)
	return err == nil && bytes.Contains(out, []byte(" zscale "))
}

func (a *App) normalizeVideo(source, target string, info videoInfo) (videoInfo, error) {
	if info.webReady() {
		if _, err := a.runVideoCommand(a.videoRemuxArgs(source, target), videoRemuxTimeout, filepath.Dir(target), false); err != nil {
			return videoInfo{}, err
		}
	} else {
		if info.HDR && !a.hasZscale() {
			info.HDR = false
		}
		if _, err := a.runVideoCommand(a.videoTranscodeArgs(source, target, info),
			time.Duration(a.Settings.VideoTranscodeTimeoutSeconds)*time.Second, filepath.Dir(target), false); err != nil {
			return videoInfo{}, err
		}
		if stat, err := os.Stat(target); err == nil && stat.Size() >= int64(a.videoMaxOutputBytes()) {
			return videoInfo{}, videoErr("The converted video is too large.")
		}
	}
	result, err := a.probeVideo(target)
	if err != nil {
		return videoInfo{}, err
	}
	if result.Codec != "h264" {
		return videoInfo{}, videoErr("The video could not be converted.")
	}
	return result, nil
}

func (a *App) extractVideoAudio(clip, target string) bool {
	args := append(videoInput(clip), "-vn", "-map", "0:a:0", "-c:a", "copy", "-movflags", "+faststart", target)
	if _, err := a.runVideoCommand(a.ffmpegArgs(args...), videoRemuxTimeout, filepath.Dir(target), false); err != nil {
		return false
	}
	stat, err := os.Stat(target)
	return err == nil && stat.Size() > 0
}

// ---- stills ------------------------------------------------------------------------------------

func videoStripPlan(durationMS int) (int, int) {
	interval := int(math.Ceil(float64(durationMS) / float64(videoStripMaxFrames)))
	if interval < videoStripMinInterval {
		interval = videoStripMinInterval
	}
	frames := int(math.Ceil(float64(durationMS) / float64(interval)))
	if frames < 1 {
		frames = 1
	}
	if frames > videoStripMaxFrames {
		frames = videoStripMaxFrames
	}
	return frames, interval
}

func (a *App) videoWebP(png []byte, quality float32, maxSide int) ([]byte, int, int, error) {
	img, _, err := decodeImageData(png)
	if err != nil {
		return nil, 0, 0, videoErr("The video's frames could not be read.")
	}
	if maxSide > 0 {
		img = fitThumbnail(img, maxSide)
	}
	data, err := encodeWebPQuality(img, quality, false)
	if err != nil {
		return nil, 0, 0, videoErr("The video's frames could not be read.")
	}
	b := img.Bounds()
	return data, b.Dx(), b.Dy(), nil
}

func (a *App) makeVideoStills(clip string, info videoInfo) (videoStills, error) {
	workdir := filepath.Dir(clip)
	posterPNG := filepath.Join(workdir, "poster.png")
	stripPNG := filepath.Join(workdir, "strip.png")
	at := math.Min(1, float64(info.DurationMS)/10000)
	args := append([]string{"-ss", fmt.Sprintf("%.3f", at)}, videoInput(clip)...)
	args = append(args, "-frames:v", "1", "-update", "1", posterPNG)
	if _, err := a.runVideoCommand(a.ffmpegArgs(args...), videoStillsTimeout, workdir, false); err != nil {
		return videoStills{}, err
	}
	posterBytes, err := os.ReadFile(posterPNG)
	if err != nil {
		return videoStills{}, videoErr("The video's frames could not be read.")
	}
	poster, _, _, err := a.videoWebP(posterBytes, videoPosterQuality, videoPosterMaxSide)
	if err != nil {
		return videoStills{}, err
	}
	posterImg, _, err := decodeImageData(poster)
	if err != nil {
		return videoStills{}, videoErr("The video's frames could not be read.")
	}
	thumb, err := encodeWebP(fitThumbnail(posterImg, 480))
	if err != nil {
		return videoStills{}, videoErr("The video's frames could not be read.")
	}
	frames, interval := videoStripPlan(info.DurationMS)
	var filmstrip []byte
	var strip map[string]any
	stripArgs := append(videoInput(clip), "-vf", fmt.Sprintf("fps=1000/%d,scale=-2:%d,tile=%dx1", interval, videoStripHeight, frames),
		"-frames:v", "1", "-update", "1", stripPNG)
	if _, err := a.runVideoCommand(a.ffmpegArgs(stripArgs...), videoStillsTimeout, workdir, false); err == nil {
		if png, rerr := os.ReadFile(stripPNG); rerr == nil {
			if data, width, height, werr := a.videoWebP(png, 75, 0); werr == nil {
				filmstrip = data
				strip = map[string]any{"frames": frames, "interval_ms": interval,
					"tile_width": width / frames, "tile_height": height}
			}
		}
	}
	return videoStills{Poster: poster, Thumbnail: thumb, Filmstrip: filmstrip, Strip: strip}, nil
}

// videoLooksLikeMedia mirrors assets.services.looks_like_media: an ISO media
// box, Matroska/WebM, or AVI. Still-image brands (AVIF/HEIC/…) are excluded.
func videoLooksLikeMedia(head []byte) bool {
	if len(head) < 12 {
		return false
	}
	switch {
	case string(head[4:8]) == "ftyp":
		brand := string(head[8:12])
		pictureBrands := map[string]bool{"avif": true, "avis": true, "heic": true, "heix": true,
			"heim": true, "heis": true, "hevc": true, "hevx": true, "hevm": true, "hevs": true,
			"mif1": true, "msf1": true}
		return !pictureBrands[brand]
	case string(head[4:8]) == "moov" || string(head[4:8]) == "mdat" || string(head[4:8]) == "free" ||
		string(head[4:8]) == "skip" || string(head[4:8]) == "wide":
		return true
	case len(head) >= 4 && string(head[:4]) == "\x1aE\xdf\xa3":
		return true
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "AVI ":
		return true
	}
	return false
}
