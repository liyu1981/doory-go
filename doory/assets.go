// User assets: list/create/retrieve/patch/delete + usage
// (apps/assets/{views,services,serializers,uploads,models}.py).
//
// Probe-first: probes/0007-assets (18 transcripts): auth required on
// everything (stock too), create201 envelope with dashed media paths and
// ORIGINAL bytes stored for clean PNGs (size == upload size), name/tag
// cleaning (path strip, control chars, collapse, lowercase dedupe), the
// invalid_file message, usage shape.
//
// Slice notes (PORT_MAP pending): SVG markup is sanitised, sized and stored
// (no separate preview: the asset URL is the preview); audio uploads still
// answer the loud notPorted error until the audio pipeline lands. AVIF
// decode and EXIF-orientation re-encode parity (Pillow encoders) are G19/G23.
// Raster thumbnails are re-encoded through libwebp at Pillow's quality 82;
// their resampling filter still differs (D15).
//
// Ports backend/apps/assets/views.py
// Ports backend/apps/assets/services.py
// Ports backend/apps/assets/serializers.py
// Ports backend/apps/assets/uploads.py
// Ports backend/apps/assets/models.py
package doory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"doory-go/internal/svg"

	_ "golang.org/x/image/webp" // decode only: sniffing + validation
)

const (
	maxUploadBytes     = 25 * 1024 * 1024 // MAX_UPLOAD_BYTES
	uploadEnvelope     = 1024 * 1024      // _ENVELOPE_BYTES (declared-size slack)
	maxImageDimension  = 12_000
	maxImagePixels     = 80_000_000
	maxAssetNameLength = 255
	maxTags            = 20
	maxTagLength       = 40
)

var assetOrdering = map[string]bool{"created_at": true, "name": true, "size": true}

// assetRow maps assets_asset.
type assetRow struct {
	ID        string
	OwnerID   string
	Kind      string
	Name      string
	Width     int64
	Height    int64
	Size      int64
	Mime      string
	Tags      string
	Meta      string
	File      string
	Thumbnail string
	// Video only (0.2.0, assets.video): a still for surfaces that can't play
	// the clip, a strip of frames for the timeline, the clip's sound on its
	// own, and the conversion state with its error.
	Poster    string
	Filmstrip string
	Audio     string
	Status    string
	Error     string
	CreatedAt string
	UpdatedAt string
}

const assetCols = `id, owner_id, kind, name, width, height, size, mime, tags, meta,
	file, thumbnail, poster, filmstrip, audio, status, error, created_at, updated_at`

func scanAsset(s scanner) (assetRow, error) {
	var a assetRow
	err := s.Scan(&a.ID, &a.OwnerID, &a.Kind, &a.Name, &a.Width, &a.Height, &a.Size,
		&a.Mime, &a.Tags, &a.Meta, &a.File, &a.Thumbnail, &a.Poster, &a.Filmstrip, &a.Audio,
		&a.Status, &a.Error, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// assetEnvelope mirrors AssetSerializer field order (url/thumb from stored
// paths; attribution/duration_ms are audio/video/stock meta — null for
// images, probe0007 step10).
type assetEnvelope struct {
	ID          string       `json:"id"`
	Kind        string       `json:"kind"`
	Name        string       `json:"name"`
	URL         string       `json:"url"`
	ThumbURL    string       `json:"thumb_url"`
	Width       int64        `json:"width"`
	Height      int64        `json:"height"`
	Size        int64        `json:"size"`
	Mime        string       `json:"mime"`
	Tags        rawCompacted `json:"tags"`
	Attribution any          `json:"attribution"`
	DurationMS  any          `json:"duration_ms"`
	Status      string       `json:"status"`
	Error       string       `json:"error"`
	PosterURL   string       `json:"poster_url"`
	AudioURL    string       `json:"audio_url"`
	Video       any          `json:"video"`
	CreatedAt   string       `json:"created_at"`
}

// pictureKinds mirrors Asset.PICTURE_KINDS: the kinds whose file is a picture
// (the thumb fallback and the default list filter use it).
var pictureKinds = map[string]bool{"image": true, "svg": true}

func (a *assetRow) envelope() assetEnvelope {
	url := ""
	if a.File != "" { // a video being converted has no file yet
		url = mediaURL + a.File
	}
	thumb := ""
	if a.Thumbnail != "" {
		thumb = mediaURL + a.Thumbnail
	} else if pictureKinds[a.Kind] && a.File != "" {
		thumb = mediaURL + a.File // fallback: the file itself (probed)
	}
	duration := any(nil)
	if meta := parseStoredMeta(a.Meta); meta != nil {
		if info, ok := meta[a.Kind].(map[string]any); ok {
			if value, ok := info["duration_ms"]; ok {
				duration = value
			}
		}
	}
	var attribution any
	if meta := parseStoredMeta(a.Meta); meta != nil {
		if value, ok := meta["attribution"]; ok {
			attribution = value
		}
	}
	poster := ""
	if a.Poster != "" {
		poster = mediaURL + a.Poster
	}
	audio := ""
	if a.Audio != "" {
		audio = mediaURL + a.Audio
	}
	var video any
	if a.Kind == "video" {
		info := map[string]any{}
		if meta := parseStoredMeta(a.Meta); meta != nil {
			if value, ok := meta["video"].(map[string]any); ok {
				info = value
			}
		}
		var strip any
		if value, ok := info["strip"].(map[string]any); ok && a.Filmstrip != "" {
			strip = map[string]any{}
			for key, value := range value {
				strip.(map[string]any)[key] = value
			}
			strip.(map[string]any)["url"] = mediaURL + a.Filmstrip
		}
		fps := info["fps"]
		if fps == nil {
			fps = 0
		}
		video = map[string]any{
			"fps":       fps,
			"has_audio": a.Audio != "",
			"filmstrip": strip,
		}
	}
	return assetEnvelope{
		ID: dashedUUID(a.ID), Kind: a.Kind, Name: a.Name,
		URL: url, ThumbURL: thumb,
		Width: a.Width, Height: a.Height, Size: a.Size, Mime: a.Mime,
		Tags:        rawCompacted(compactStoredJSON([]byte(a.Tags))),
		Attribution: attribution, DurationMS: duration,
		Status: a.Status, Error: a.Error,
		PosterURL: poster, AudioURL: audio, Video: video,
		CreatedAt: renderDRFDatetime(a.CreatedAt),
	}
}

// parseStoredMeta decodes meta/tags JSON into a map (nil when not an object,
// like the serializer's isinstance(dict) guards).
func parseStoredMeta(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil
	}
	return value
}

// assetsList ports AssetViewSet.list: kind (default excludes audio), q
// (icontains name), ordering whitelist, pagination 24 (create via POST).
func (a *App) assetsList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.assetsCreate(w, r)
		return
	}
	u := currentUser(r)
	q := r.URL.Query()
	var fields []FieldError

	where := []string{"owner_id = ?"}
	args := []any{u.ID}

	if kind := q.Get("kind"); kind != "" {
		if !choiceValid([]string{"image", "svg", "audio", "video"}, kind) {
			fields = append(fields, SimpleField("kind",
				fmt.Sprintf("%q is not a valid choice.", kind), "invalid_choice"))
		} else {
			where = append(where, "kind = ?")
			args = append(args, kind)
		}
	} else {
		// 0.2.0: the default list is the picture kinds (audio and video
		// only by their own kind).
		where = append(where, `kind IN ('image', 'svg')`)
	}
	if search := q.Get("q"); search != "" {
		if len([]rune(search)) > 100 {
			fields = append(fields, SimpleField("q",
				"Ensure this field has no more than 100 characters.", "max_length"))
		} else {
			where = append(where, "name LIKE ? ESCAPE '\\'")
			args = append(args, "%"+escapeLike(strings.TrimSpace(search))+"%")
		}
	}
	ordering := q.Get("ordering")
	if len([]rune(ordering)) > 32 {
		fields = append(fields, SimpleField("ordering",
			"Ensure this field has no more than 32 characters.", "max_length"))
	} else if ordering != "" && !assetOrdering[strings.TrimPrefix(ordering, "-")] {
		fields = append(fields, SimpleField("ordering",
			"Unsupported ordering. Use one of: created_at, name, size (prefix - for descending).",
			"invalid"))
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	order := "created_at DESC, id ASC" // default + order_queryset pk tie-break
	if ordering != "" {
		col := strings.TrimPrefix(ordering, "-")
		dirn := "ASC"
		if strings.HasPrefix(ordering, "-") {
			dirn = "DESC"
		}
		order = col + " " + dirn + ", id ASC"
	}

	rows, err := a.DB.Query(
		"SELECT "+assetCols+" FROM assets_asset WHERE "+strings.Join(where, " AND ")+
			" ORDER BY "+order, args...)
	if err != nil {
		logerr(err, "assetsList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	list := []assetRow{}
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			logerr(err, "assetsList.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		list = append(list, asset)
	}
	page, ferr := windowSlice(r, len(list), designPageSize)
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	from, to := clampWindow(len(list), page)
	envs := make([]assetEnvelope, 0, to-from)
	for _, asset := range list[from:to] {
		envs = append(envs, asset.envelope())
	}
	resp, err := paginateSized(r, len(list), envs, designPageSize)
	if err != nil {
		if de, ok := err.(*DocumentError); ok {
			writeDRF(w, de.Status, de.Code, de.Message)
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func clampWindow(total int, b pageBounds) (int, int) {
	from, to := b.from, b.to
	if from > total {
		from = total
	}
	if to > total {
		to = total
	}
	return from, to
}

// assetsCreate ports AssetViewSet.create: declared-size reject, serializer
// (file/name/tags), content-driven prepare, quota, storage.
func (a *App) assetsCreate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	// reject_oversize_request: declared size beyond limit+envelope fails
	// before buffering.
	if r.ContentLength > maxUploadBytes+uploadEnvelope {
		writeDRF(w, http.StatusRequestEntityTooLarge, "file_too_large", "The file is too large.")
		return
	}
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	var fields []FieldError
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		fields = append(fields, SimpleField("file", "This field is required.", "required"))
	}
	var nameRaw string
	if v := r.MultipartForm.Value["name"]; len(v) > 0 {
		if len([]rune(v[0])) > maxAssetNameLength*2 {
			fields = append(fields, SimpleField("name",
				"Ensure this field has no more than 510 characters.", "max_length"))
		} else {
			nameRaw = v[0]
		}
	}
	var tags []string
	tagSeen := map[string]bool{}
	if values, ok := r.MultipartForm.Value["tags"]; ok {
		if len(values) > maxTags {
			fields = append(fields, SimpleField("tags",
				"Ensure this field has no more than 20 elements.", "max_length"))
		}
		for _, v := range values {
			if len([]rune(v)) > maxTagLength {
				fields = append(fields, SimpleField("tags",
					"Ensure this field has no more than 40 characters.", "max_length"))
				continue
			}
			cleaned := cleanTag(v)
			if cleaned != "" && !tagSeen[cleaned] {
				tagSeen[cleaned] = true
				tags = append(tags, cleaned)
			}
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	if len(files) == 0 {
		return
	}

	fh := files[0]
	handle, err := fh.Open()
	if err != nil {
		logerr(err, "assetsCreate.open")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	data, err := readBounded(handle, maxUploadBytes)
	handle.Close()
	if err != nil {
		writeDRF(w, http.StatusRequestEntityTooLarge, "file_too_large", "The file is too large.")
		return
	}
	sourceName := nameRaw
	if sourceName == "" {
		sourceName = fh.Filename
	}
	name := cleanAssetName(sourceName, "Untitled")

	prepared, perr := prepareFile(data)
	if perr != nil {
		writeDRF(w, perr.Status, perr.Code, perr.Message)
		return
	}

	asset := assetRow{
		ID: newUUID4Hex(), OwnerID: u.ID, Kind: prepared.kind, Name: name,
		Width: int64(prepared.width), Height: int64(prepared.height),
		Size: int64(len(prepared.data)), Mime: prepared.mime,
		Tags: mustMarshalString(tags), Meta: "{}", Status: "ready",
	}
	// storage: upload_to uses uuid STRING values (probe0007 step10):
	// assets/<owner-dashed>/<id-dashed>.<ext> + thumbs/<id>.webp
	asset.File = "assets/" + dashedUUID(u.ID) + "/" + dashedUUID(asset.ID) + "." + prepared.ext
	if prepared.kind == "image" && prepared.thumb != nil {
		asset.Thumbnail = "assets/" + dashedUUID(u.ID) + "/thumbs/" + dashedUUID(asset.ID) + ".webp"
	}

	used, _ := a.storageUsed(u.ID)
	if used+asset.Size > a.Settings.UserStorageQuotaBytes {
		writeDRF(w, http.StatusRequestEntityTooLarge, "quota_exceeded",
			"Your storage quota is full. Delete some uploads to free up space.")
		return
	}

	dir := filepath.Join(a.Settings.MediaRoot, filepath.Dir(asset.File))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logerr(err, "assetsCreate.mkdir")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if err := os.WriteFile(filepath.Join(a.Settings.MediaRoot, asset.File), prepared.data, 0o644); err != nil {
		logerr(err, "assetsCreate.file")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	// SVGs have no separate thumbnail (the asset URL is the preview).
	if asset.Thumbnail != "" {
		thumbPath := filepath.Join(a.Settings.MediaRoot, asset.Thumbnail)
		if err := os.MkdirAll(filepath.Dir(thumbPath), 0o755); err == nil {
			_ = os.WriteFile(thumbPath, prepared.thumb, 0o644)
		}
	}

	now := formatDjangoDateTime(time.Now())
	if _, err := a.DB.Exec(
		"INSERT INTO assets_asset ("+assetCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		asset.ID, asset.OwnerID, asset.Kind, asset.Name, asset.Width, asset.Height, asset.Size,
		asset.Mime, asset.Tags, asset.Meta, asset.File, asset.Thumbnail,
		asset.Poster, asset.Filmstrip, asset.Audio, asset.Status, asset.Error, now, now); err != nil {
		logerr(err, "assetsCreate.insert")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	created, err := a.getAssetByID(asset.ID)
	if err != nil || created == nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusCreated, created.envelope())
}

// preparedRaster mirrors core/images.process_image_upload for the ported
// formats: DECODE decides (never the filename); dimension/pixel bounds;
// clean files store their ORIGINAL bytes (probe0007: size == upload size).
type preparedRaster struct {
	kind   string
	data   []byte
	mime   string
	ext    string
	width  int
	height int
	thumb  []byte
}

func prepareRaster(data []byte) (*preparedRaster, *DocumentError) {
	if len(data) == 0 {
		return nil, &DocumentError{Code: "invalid_file", Status: 400, Message: "The file is empty."}
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// Content decides the type: markup/audio answer loud notPorted
		// errors (their pipelines are family-pending), everything else gets
		// the probe-proven unsupported message.
		return nil, &DocumentError{Code: "invalid_file", Status: 400,
			Message: "Unsupported image type. Use PNG, JPEG, WebP, GIF or AVIF."}
	}
	width, height := cfg.Width, cfg.Height
	if width < 1 || height < 1 || width > maxImageDimension || height > maxImageDimension ||
		int64(width)*int64(height) > maxImagePixels {
		return nil, &DocumentError{Code: "invalid_file", Status: 400,
			Message: "The image dimensions are too large."}
	}
	// Full decode surfaces truncated/corrupt files (DecodeConfig reads only
	// the header).
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, &DocumentError{Code: "invalid_file", Status: 400,
			Message: "The file is not a valid image."}
	}
	mimes := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}
	exts := map[string]string{"png": "png", "jpeg": "jpg", "gif": "gif", "webp": "webp"}
	mime, ext := mimes[format], exts[format]
	if mime == "" {
		// Formats Pillow accepts (AVIF) that Go cannot decode yet: pending
		// in PORT_MAP (assets rows).
		return nil, &DocumentError{Code: "invalid_file", Status: 400,
			Message: "Unsupported image type. Use PNG, JPEG, WebP, GIF or AVIF."}
	}
	prepared := &preparedRaster{kind: "image", data: data, mime: mime, ext: ext,
		width: width, height: height}
	// _prepare_raster: a 480 px WebP thumbnail, optional when encoding fails.
	if thumb, terr := encodeWebP(fitThumbnail(img, thumbnailSide)); terr == nil {
		prepared.thumb = thumb
	}
	return prepared, nil
}

// prepareSVG ports assets.services._prepare_svg: sanitise, inspect the
// intrinsic size, cap it at MAX_SVG_DIMENSION and store the sanitised bytes.
func prepareSVG(data []byte) (*preparedRaster, *DocumentError) {
	if len(data) > svg.MaxBytes {
		return nil, &DocumentError{Code: "file_too_large", Status: 413,
			Message: "The SVG is too large."}
	}
	clean, err := svg.Sanitize(data, false)
	if err != nil {
		return nil, &DocumentError{Code: "invalid_file", Status: 400,
			Message: "The file is not a valid, safe SVG."}
	}
	info, err := svg.Info(clean)
	if err != nil {
		return nil, &DocumentError{Code: "invalid_file", Status: 400,
			Message: "The file is not a valid, safe SVG."}
	}
	scale := 1.0
	largest := math.Max(math.Max(info.Width, info.Height), 1)
	if largest > maxImageDimension {
		scale = maxImageDimension / largest
	}
	width := int(math.Max(1, math.Round(info.Width*scale)))
	height := int(math.Max(1, math.Round(info.Height*scale)))
	clean, err = svg.WithIntrinsicSize(clean, width, height)
	if err != nil {
		return nil, &DocumentError{Code: "invalid_file", Status: 400,
			Message: "The file is not a valid, safe SVG."}
	}
	return &preparedRaster{kind: "svg", data: clean, mime: "image/svg+xml", ext: "svg",
		width: width, height: height}, nil
}

// prepareFile ports assets.services.prepare_file: content, never the
// filename, decides the type.
func prepareFile(data []byte) (*preparedRaster, *DocumentError) {
	if len(data) == 0 {
		return nil, &DocumentError{Code: "invalid_file", Status: 400, Message: "The file is empty."}
	}
	if looksLikeAudio(data) {
		return nil, &DocumentError{Code: "server_error", Status: 500,
			Message: "route registered but not ported: assets._prepare_audio (assets/audio.py pending)"}
	}
	if looksLikeMarkup(data) {
		return prepareSVG(data)
	}
	return prepareRaster(data)
}

// looksLikeMarkup mirrors services._looks_like_markup (BOM, first 2 KiB,
// lstrip whitespace -> "<").
func looksLikeMarkup(data []byte) bool {
	head := data
	if bytes.HasPrefix(head, []byte{0xEF, 0xBB, 0xBF}) {
		head = head[3:]
	}
	if len(head) > 2048 {
		head = head[:2048]
	}
	return bytes.HasPrefix(bytes.TrimLeft(head, " \\t\\n\\r"), []byte("<"))
}

// looksLikeAudio: WAV/MP3/OGG/FLAC magic — content decides the type (like
// assets/audio.py). The full audio pipeline is family-pending; detection
// routes uploads to the loud notPorted error instead of a wrong message.
func looksLikeAudio(data []byte) bool {
	if bytes.HasPrefix(data, []byte("RIFF")) && len(data) >= 12 && bytes.Equal(data[8:12], []byte("WAVE")) {
		return true
	}
	if bytes.HasPrefix(data, []byte("ID3")) || bytes.HasPrefix(data, []byte{0xFF, 0xFB}) ||
		bytes.HasPrefix(data, []byte{0xFF, 0xF3}) || bytes.HasPrefix(data, []byte{0xFF, 0xF6}) {
		return true
	}
	return bytes.HasPrefix(data, []byte("OggS")) || bytes.HasPrefix(data, []byte("fLaC"))
}

func (a *App) storageUsed(ownerID string) (int64, error) {
	var total sql.NullInt64
	err := a.DB.QueryRow(
		`SELECT SUM(size) FROM assets_asset WHERE owner_id = ?`, ownerID).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

func (a *App) getAssetByID(id string) (*assetRow, error) {
	row := a.DB.QueryRow("SELECT "+assetCols+" FROM assets_asset WHERE id = ?", id)
	asset, err := scanAsset(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &asset, nil
}

func (a *App) getAssetFor(w http.ResponseWriter, r *http.Request) (*assetRow, bool) {
	u := currentUser(r)
	id := hexUUID(routeParam(r, "uuid"))
	asset, err := a.getAssetByID(id)
	if err != nil {
		logerr(err, "getAssetFor")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return nil, false
	}
	if asset == nil || asset.OwnerID != u.ID {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, false
	}
	return asset, true
}

// assetsRetrieve ports RetrieveModelMixin.
func (a *App) assetsRetrieve(w http.ResponseWriter, r *http.Request) {
	asset, ok := a.getAssetFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, asset.envelope())
}

// assetsPatch ports partial_update (name/tags with cleaning).
func (a *App) assetsPatch(w http.ResponseWriter, r *http.Request) {
	asset, ok := a.getAssetFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Name *string   `json:"name"`
		Tags *[]string `json:"tags"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError
	if body.Name != nil {
		cleaned := cleanAssetName(*body.Name, "")
		if cleaned == "" {
			fields = append(fields, SimpleField("name", "The name cannot be empty.", "invalid"))
		} else {
			body.Name = &cleaned
		}
	}
	var cleanedTags []string
	if body.Tags != nil {
		if len(*body.Tags) > maxTags {
			fields = append(fields, SimpleField("tags",
				"Ensure this field has no more than 20 elements.", "max_length"))
		}
		cleanedTags = []string{}
		seen := map[string]bool{}
		for _, v := range *body.Tags {
			if len([]rune(v)) > maxTagLength {
				fields = append(fields, SimpleField("tags",
					"Ensure this field has no more than 40 characters.", "max_length"))
				continue
			}
			c := cleanTag(v)
			if c != "" && !seen[c] {
				seen[c] = true
				cleanedTags = append(cleanedTags, c)
			}
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	updates := []string{}
	args := []any{}
	if body.Name != nil {
		updates = append(updates, "name = ?")
		args = append(args, *body.Name)
	}
	if body.Tags != nil {
		updates = append(updates, "tags = ?")
		args = append(args, mustMarshalString(cleanedTags))
	}
	if len(updates) == 0 {
		writeJSON(w, http.StatusOK, asset.envelope())
		return
	}
	updates = append(updates, "updated_at = ?")
	args = append(args, formatDjangoDateTime(time.Now()))
	args = append(args, asset.ID, asset.OwnerID)
	if _, err := a.DB.Exec(
		"UPDATE assets_asset SET "+strings.Join(updates, ", ")+
			" WHERE id = ? AND owner_id = ?", args...); err != nil {
		logerr(err, "assetsPatch")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	fresh, err := a.getAssetByID(asset.ID)
	if err != nil || fresh == nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, fresh.envelope())
}

// assetsDestroy ports DestroyModelMixin: rows + files.
func (a *App) assetsDestroy(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	asset, ok := a.getAssetFor(w, r)
	if !ok {
		return
	}
	_ = os.Remove(filepath.Join(a.Settings.MediaRoot, asset.File))
	if asset.Thumbnail != "" {
		_ = os.Remove(filepath.Join(a.Settings.MediaRoot, asset.Thumbnail))
	}
	if _, err := a.DB.Exec(`DELETE FROM assets_asset WHERE id = ? AND owner_id = ?`,
		asset.ID, u.ID); err != nil {
		logerr(err, "assetsDestroy")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// assetsUsage ports the usage action (GET /assets/usage/): order probed
// (used_bytes then quota_bytes).
func (a *App) assetsUsage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	used, err := a.storageUsed(u.ID)
	if err != nil {
		logerr(err, "assetsUsage")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, usageEnvelope{
		UsedBytes: used, QuotaBytes: a.Settings.UserStorageQuotaBytes,
	})
}

type usageEnvelope struct {
	UsedBytes  int64 `json:"used_bytes"`
	QuotaBytes int64 `json:"quota_bytes"`
}

// cleanAssetName ports uploads.clean_name: backslash->slash then the last
// segment, NFC, control/invisible stripped, whitespace collapsed, trimmed of
// spaces/dots, clipped to 255, fallback.
func cleanAssetName(raw, fallback string) string {
	slashed := strings.ReplaceAll(raw, "\\", "/")
	if idx := strings.LastIndexByte(slashed, '/'); idx >= 0 {
		slashed = slashed[idx+1:]
	}
	name := strings.Join(strings.Fields(stripUnsafe(slashed)), " ")
	name = strings.Trim(name, " .")
	runes := []rune(name)
	if len(runes) > maxAssetNameLength {
		runes = runes[:maxAssetNameLength]
	}
	name = strings.TrimSpace(string(runes))
	if name == "" {
		return fallback
	}
	return name
}

// cleanTag ports uploads.clean_tags: stripped, collapsed, lowercased,
// de-duplicated, empties dropped.
func cleanTag(v string) string {
	return strings.ToLower(strings.Join(strings.Fields(stripUnsafe(v)), " "))
}

func stripUnsafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isControlCat(r) || isInvisible(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > int(limit) {
		return nil, errTooLarge
	}
	return data, nil
}

type tooLargeError struct{}

func (tooLargeError) Error() string { return "The file is too large." }

var errTooLarge = tooLargeError{}

func mustMarshalString(list []string) string {
	if list == nil {
		list = []string{}
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}
