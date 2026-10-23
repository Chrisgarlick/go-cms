package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"

	"cms/internal/db/dbq"
	"cms/internal/jobs"
)

// Job describes one resize for a processor.
type Job struct {
	Src     string  `json:"src"`
	Dst     string  `json:"dst"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Fit     string  `json:"fit"`
	Format  string  `json:"format"`
	FocalX  float64 `json:"focal_x"`
	FocalY  float64 `json:"focal_y"`
	Quality int     `json:"quality"`
	MaxPx   int     `json:"max_px"`
}

type Result struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Processor turns one original into one variant in a separate process.
type Processor interface {
	Name() string
	Format(srcMime string) string // output format for a source type
	Run(ctx context.Context, j Job) (Result, error)
}

// Vips uses the libvips command-line tools (apt install libvips-tools).
type Vips struct {
	Binary string // "vips"
}

func (v *Vips) Name() string { return "vips" }

func (v *Vips) Format(string) string { return "webp" }

func (v *Vips) Run(ctx context.Context, j Job) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	q := j.Quality
	if q == 0 {
		q = 80
	}
	w, h := j.Width, j.Height
	if w == 0 {
		w = 10000000
	}
	args := []string{"thumbnail", j.Src, fmt.Sprintf("%s[Q=%d,strip]", j.Dst, q), strconv.Itoa(w), "--size", "down"}
	if h > 0 {
		args = append(args, "--height", strconv.Itoa(h))
	}
	if j.Fit == "cover" && h > 0 {
		args = append(args, "--crop", "attention")
	}
	cmd := exec.CommandContext(ctx, v.Binary, args...)
	cmd.Env = []string{"VIPS_CONCURRENCY=1", "PATH=/usr/local/bin:/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		return Result{}, fmt.Errorf("vips: %v: %s", err, bytes.TrimSpace(out))
	}
	f, err := os.Open(j.Dst)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	cfg, err := webp.DecodeConfig(f)
	if err != nil {
		return Result{}, fmt.Errorf("vips: unreadable output: %w", err)
	}
	return Result{Width: cfg.Width, Height: cfg.Height}, nil
}

// Child re-executes this binary (`cms media-process`) so decoding happens in a
// throwaway process with its own memory limit. Pure Go: JPEG and PNG output.
type Child struct {
	Exe string // os.Executable()
}

func (c *Child) Name() string { return "builtin" }

func (c *Child) Format(srcMime string) string {
	if srcMime == "image/png" || srcMime == "image/gif" || srcMime == "image/webp" {
		return "png"
	}
	return "jpg"
}

func (c *Child) Run(ctx context.Context, j Job) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	in, _ := json.Marshal(j)
	cmd := exec.CommandContext(ctx, c.Exe, "media-process")
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = []string{"GOMEMLIMIT=200MiB", "GOGC=50"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("media-process: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	var r Result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		return Result{}, fmt.Errorf("media-process: bad output: %w", err)
	}
	return r, nil
}

// RunChild is the body of `cms media-process`: read a Job on stdin, write a
// Result on stdout. It runs in its own process and exits after one image.
func RunChild(stdin io.Reader, stdout io.Writer) error {
	debug.SetMemoryLimit(200 << 20)
	var j Job
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<16)).Decode(&j); err != nil {
		return err
	}
	if j.MaxPx == 0 {
		j.MaxPx = 40_000_000
	}
	f, err := os.Open(j.Src)
	if err != nil {
		return err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return err
	}
	if cfg.Width*cfg.Height > j.MaxPx {
		return ErrTooManyPx
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	orientation := 1
	if head := make([]byte, 64<<10); true {
		n, _ := io.ReadFull(f, head)
		orientation = jpegOrientation(head[:n])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	src = orient(src, orientation)
	dst := resize(src, j)
	out, err := os.Create(j.Dst)
	if err != nil {
		return err
	}
	q := j.Quality
	if q == 0 {
		q = 82
	}
	switch j.Format {
	case "png":
		err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(out, dst)
	default:
		err = jpeg.Encode(out, dst, &jpeg.Options{Quality: q})
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(j.Dst)
		return err
	}
	b := dst.Bounds()
	return json.NewEncoder(stdout).Encode(Result{Width: b.Dx(), Height: b.Dy()})
}

func resize(src image.Image, j Job) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	tw, th := j.Width, j.Height
	if j.Fit == "cover" && tw > 0 && th > 0 {
		// Crop to the target aspect ratio around the focal point, then scale.
		tr := float64(tw) / float64(th)
		cw, ch := sw, int(float64(sw)/tr)
		if ch > sh {
			ch, cw = sh, int(float64(sh)*tr)
		}
		fx, fy := j.FocalX, j.FocalY
		if fx == 0 && fy == 0 {
			fx, fy = 0.5, 0.5
		}
		x0 := clampInt(int(fx*float64(sw))-cw/2, 0, sw-cw)
		y0 := clampInt(int(fy*float64(sh))-ch/2, 0, sh-ch)
		crop := image.Rect(sb.Min.X+x0, sb.Min.Y+y0, sb.Min.X+x0+cw, sb.Min.Y+y0+ch)
		if tw > cw {
			tw, th = cw, ch
		}
		dst := image.NewRGBA(image.Rect(0, 0, tw, th))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, crop, draw.Src, nil)
		return dst
	}
	// Inside: fit within the box, never upscale.
	scale := 1.0
	if tw > 0 && sw > tw {
		scale = float64(tw) / float64(sw)
	}
	if th > 0 && float64(sh)*scale > float64(th) {
		scale = float64(th) / float64(sh)
	}
	if scale >= 1 {
		dst := image.NewRGBA(image.Rect(0, 0, sw, sh))
		draw.Draw(dst, dst.Bounds(), src, sb.Min, draw.Src)
		return dst
	}
	nw, nh := max(1, int(float64(sw)*scale+0.5)), max(1, int(float64(sh)*scale+0.5))
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
	return dst
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// jpegOrientation reads the EXIF orientation tag (1-8) from a JPEG header.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if marker == 0xE1 && i+4+size <= len(b) && size > 8 && string(b[i+4:i+10]) == "Exif\x00\x00" {
			return exifOrientation(b[i+10 : i+2+size])
		}
		if marker == 0xDA {
			return 1
		}
		i += 2 + size
	}
	return 1
}

func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			v := int(bo.Uint16(t[e+8:]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

func orient(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	swap := o >= 5
	dw, dh := w, h
	if swap {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			}
			dst.Set(nx, ny, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// DetectProcessor prefers vips when installed, else the builtin child.
func DetectProcessor(vipsBinary string) Processor {
	if vipsBinary != "" {
		if p, err := exec.LookPath(vipsBinary); err == nil {
			return &Vips{Binary: p}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return &Child{Exe: exe}
}

// Process generates every preset for one image (the media.process job).
// Idempotent: existing variants are skipped unless force is set.
func (s *Service) Process(ctx context.Context, args jobs.MediaProcess) error {
	id, err := uuid.Parse(args.MediaID)
	if err != nil {
		return nil // malformed job: nothing to retry
	}
	m, err := s.DB.Q.GetMedia(ctx, id)
	if err != nil {
		return nil // deleted since upload
	}
	if !strings.HasPrefix(m.Mime, "image/") || m.Mime == "image/svg+xml" {
		return nil
	}
	src, err := s.Store.LocalPath(m.StorageKey)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	if !args.Force {
		vs, err := s.DB.Q.VariantsFor(ctx, []uuid.UUID{id})
		if err != nil {
			return err
		}
		for _, v := range vs {
			existing[v.Preset] = true
		}
	}
	format := s.Processor.Format(m.Mime)
	if m.Mime == "image/avif" {
		if _, ok := s.Processor.(*Vips); !ok {
			return errors.New("AVIF originals need libvips (apt install libvips-tools)")
		}
	}
	for _, p := range s.Presets() {
		if existing[p.Name] {
			continue
		}
		key := variantKey(id.String(), p.Name, format)
		dst, err := s.Store.LocalPath(key)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		tmp := dst + ".tmp." + format
		res, err := s.Processor.Run(ctx, Job{Src: src, Dst: tmp, Width: p.Width, Height: p.Height, Fit: p.Fit,
			Format: format, FocalX: float64(m.FocalX), FocalY: float64(m.FocalY), MaxPx: s.MaxPixels})
		if err != nil {
			os.Remove(tmp)
			return fmt.Errorf("preset %s: %w", p.Name, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
		st, err := os.Stat(dst)
		if err != nil {
			return err
		}
		if err := s.DB.Q.UpsertVariant(ctx, dbq.UpsertVariantParams{MediaID: id, Preset: p.Name, Format: format,
			StorageKey: key, Width: int32(res.Width), Height: int32(res.Height), Size: st.Size()}); err != nil {
			return err
		}
	}
	// Pages showing this image can now use real variants.
	users, err := s.DB.Q.EntriesUsingMedia(ctx, id)
	if err != nil {
		return err
	}
	deps := []jobs.Dep{{Type: "media", ID: id.String()}}
	for _, e := range users {
		deps = append(deps, jobs.Dep{Type: "entry", ID: e.String()})
	}
	return s.Jobs.Enqueue(ctx, jobs.RenderInvalidate{Deps: deps})
}
