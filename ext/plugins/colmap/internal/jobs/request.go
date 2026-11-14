package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/oai"
	"github.com/iodesystems/corrallm/ext/plugins/colmap/internal/priors"
)

// ImageEntry is one entry of the create request's images[].
type ImageEntry struct {
	Name   string `json:"name,omitempty"`
	FileID string `json:"file_id,omitempty"`
	priors.Spec
}

// Camera is the create request's camera object.
type Camera struct {
	Model        string `json:"model,omitempty"`
	SingleCamera bool   `json:"single_camera,omitempty"`
}

// Request is POST /v1/reconstructions (spec §4.4).
type Request struct {
	Model                 string            `json:"model"`
	ArchiveFileID         string            `json:"archive_file_id,omitempty"`
	Images                []ImageEntry      `json:"images,omitempty"`
	EXIFGPS               *bool             `json:"exif_gps,omitempty"`
	DefaultPositionSigmaM *float64          `json:"default_position_sigma_m,omitempty"`
	FrameOrigin           *priors.LLA       `json:"frame_origin,omitempty"`
	Camera                *Camera           `json:"camera,omitempty"`
	Matcher               string            `json:"matcher,omitempty"`
	Mapper                string            `json:"mapper,omitempty"`
	MaxImageSize          int               `json:"max_image_size,omitempty"`
	Dense                 bool              `json:"dense,omitempty"`
	Mesh                  *string           `json:"mesh,omitempty"`
	FPS                   float64           `json:"fps,omitempty"`
	Metadata              map[string]string `json:"metadata,omitempty"`
}

// Limits (D8 and spec §4.4).
const (
	MinImages            = 3
	DefaultMaxImages     = 5000
	MaxExhaustiveImages  = 1000
	DefaultMaxImageSize  = 3200
	DefaultFPS           = 2.0
	MaxMetadataPairs     = 16
	MaxMetadataKeyLen    = 64
	MaxMetadataValueLen  = 512
	DefaultCameraModel   = "SIMPLE_RADIAL"
	DefaultMatcher       = "exhaustive"
	DefaultMapper        = "incremental"
	MaxQueuedPerOwner    = 10
	DefaultOutputTTLDays = 7
)

// COLMAP 4.2.1 camera model names (sensor/models.h).
var cameraModels = map[string]bool{
	"SIMPLE_PINHOLE": true, "PINHOLE": true, "SIMPLE_RADIAL": true, "RADIAL": true, "OPENCV": true,
	"OPENCV_FISHEYE": true, "FULL_OPENCV": true, "FOV": true, "SIMPLE_RADIAL_FISHEYE": true,
	"RADIAL_FISHEYE": true, "THIN_PRISM_FISHEYE": true, "RAD_TAN_THIN_PRISM_FISHEYE": true,
	"SIMPLE_DIVISION": true, "DIVISION": true, "SIMPLE_FISHEYE": true, "FISHEYE": true,
}

// ParseRequest decodes a JSON create body. Unknown fields are refused, as
// OpenAI does.
func ParseRequest(r io.Reader) (*Request, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			return nil, oai.Invalid(ute.Field, "%s: expected %s", ute.Field, ute.Type)
		}
		if f, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			f = strings.Trim(f, `"`)
			return nil, oai.Invalid(f, "unrecognized request argument supplied: %s", f)
		}
		return nil, oai.Invalid("", "invalid JSON body: %v", err)
	}
	if dec.More() {
		return nil, oai.Invalid("", "invalid JSON body: trailing data")
	}
	return &req, nil
}

// Normalize fills defaults and checks every field that needs no file lookup.
func (r *Request) Normalize(maxImages int) error {
	if r.Model == "" {
		return oai.Invalid("model", "model is required")
	}
	switch {
	case r.ArchiveFileID == "" && len(r.Images) == 0:
		return oai.Invalid("images", "give archive_file_id or images[] with file_id")
	case r.ArchiveFileID != "":
		for i, im := range r.Images {
			if im.FileID != "" {
				return oai.Invalid(fmt.Sprintf("images[%d].file_id", i), "with archive_file_id, images[] is per-image metadata matched by name; file_id is not allowed")
			}
			if im.Name == "" {
				return oai.Invalid(fmt.Sprintf("images[%d].name", i), "name is required with archive_file_id")
			}
		}
	default:
		for i, im := range r.Images {
			if im.FileID == "" {
				return oai.Invalid(fmt.Sprintf("images[%d].file_id", i), "file_id is required without archive_file_id")
			}
		}
	}
	if len(r.Images) > maxImages {
		return oai.Invalid("images", "at most %d images per job", maxImages)
	}
	if r.EXIFGPS == nil {
		t := true
		r.EXIFGPS = &t
	}
	if r.DefaultPositionSigmaM != nil && !(*r.DefaultPositionSigmaM > 0) {
		return oai.Invalid("default_position_sigma_m", "must be > 0 (metres)")
	}
	if o := r.FrameOrigin; o != nil {
		if o.Lat < -90 || o.Lat > 90 || o.Lon < -180 || o.Lon > 180 {
			return oai.Invalid("frame_origin", "frame_origin needs lat in [-90, 90] and lon in [-180, 180]")
		}
	}
	if r.Camera == nil {
		r.Camera = &Camera{}
	}
	if r.Camera.Model == "" {
		r.Camera.Model = DefaultCameraModel
	}
	if !cameraModels[r.Camera.Model] {
		return oai.Invalid("camera.model", "unknown COLMAP camera model %q", r.Camera.Model)
	}
	if r.Matcher == "" {
		r.Matcher = DefaultMatcher
	}
	switch r.Matcher {
	case "exhaustive", "sequential", "spatial":
	default:
		return oai.Invalid("matcher", "matcher must be exhaustive, sequential or spatial")
	}
	if r.Mapper == "" {
		r.Mapper = DefaultMapper
	}
	switch r.Mapper {
	case "incremental", "global":
	default:
		return oai.Invalid("mapper", "mapper must be incremental or global")
	}
	if r.MaxImageSize == 0 {
		r.MaxImageSize = DefaultMaxImageSize
	}
	if r.MaxImageSize < 100 || r.MaxImageSize > 16384 {
		return oai.Invalid("max_image_size", "max_image_size must be 100..16384 px")
	}
	if r.Mesh != nil {
		switch *r.Mesh {
		case "poisson", "delaunay":
		default:
			return oai.Invalid("mesh", "mesh must be null, \"poisson\" or \"delaunay\"")
		}
		if !r.Dense {
			return oai.Invalid("mesh", "mesh needs dense: true")
		}
	}
	if r.FPS == 0 {
		r.FPS = DefaultFPS
	}
	if r.FPS < 0.01 || r.FPS > 60 {
		return oai.Invalid("fps", "fps must be 0.01..60 frames per second")
	}
	if len(r.Metadata) > MaxMetadataPairs {
		return oai.Invalid("metadata", "at most %d metadata pairs", MaxMetadataPairs)
	}
	for k, v := range r.Metadata {
		if len(k) > MaxMetadataKeyLen || len(v) > MaxMetadataValueLen {
			return oai.Invalid("metadata", "metadata keys are ≤ %d characters and values ≤ %d", MaxMetadataKeyLen, MaxMetadataValueLen)
		}
	}
	return nil
}
