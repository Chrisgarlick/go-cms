package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"cms/internal/actor"
	"cms/internal/jobs"
	"cms/internal/media"
	"cms/internal/rbac"
	"cms/internal/site"
	"cms/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func pngBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func TestSniff(t *testing.T) {
	cases := []struct {
		head []byte
		name string
		ok   bool
	}{
		{pngBytes(4, 4), "a.png", true},
		{pngBytes(4, 4), "a.jpg", false}, // extension lies
		{[]byte("<svg xmlns='http://www.w3.org/2000/svg'></svg>"), "a.svg", true},
		{[]byte("<html><script>alert(1)</script>"), "a.png", false},
		{[]byte("%PDF-1.7 ..."), "doc.pdf", true},
		{[]byte("MZ\x90\x00 executable"), "setup.exe", false},
	}
	for i, c := range cases {
		_, _, _, err := media.Sniff(c.head, c.name)
		if (err == nil) != c.ok {
			t.Errorf("case %d (%s): err=%v", i, c.name, err)
		}
	}
}

func TestSafeFilename(t *testing.T) {
	for in, want := range map[string]string{"../../etc/passwd.png": "passwd.png", "My Photo (1).JPG": "my-photo-1.jpg", "日本.png": "file.png"} {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(in), "."))
		if got := media.SafeFilename(in, ext); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// inProcess runs RunChild through pipes, standing in for the subprocess.
type inProcess struct{}

func (inProcess) Name() string         { return "test" }
func (inProcess) Format(string) string { return "jpg" }
func (inProcess) Run(_ context.Context, j media.Job) (media.Result, error) {
	in, _ := json.Marshal(j)
	var out bytes.Buffer
	if err := media.RunChild(bytes.NewReader(in), &out); err != nil {
		return media.Result{}, err
	}
	var r media.Result
	return r, json.Unmarshal(out.Bytes(), &r)
}

func TestUploadAndProcess(t *testing.T) {
	d := testutil.NewDB(t)
	dir := t.TempDir()
	b := testutil.BlogBundle(t)
	j := &jobs.Discard{}
	svc := &media.Service{DB: d.DB, Store: &media.Local{Root: dir}, Site: func() *site.Bundle { return b }, Jobs: j,
		TmpDir: filepath.Join(dir, "tmp"), BaseURL: "https://example.test/media", MaxBytes: 5 << 20, MaxPixels: 10_000_000, Processor: inProcess{}}
	ctx := actor.With(context.Background(), &actor.Actor{Kind: actor.KindSystem, Caps: map[rbac.Cap]bool{rbac.MediaManage: true, rbac.ContentRead: true}, Label: "t"})
	var jb bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2000, 1000))
	_ = jpeg.Encode(&jb, img, nil)
	m, dup, err := svc.Upload(ctx, media.UploadInput{Filename: "Wide Shot.jpg", Body: bytes.NewReader(jb.Bytes())})
	if err != nil || dup {
		t.Fatalf("upload: %v dup=%v", err, dup)
	}
	if *m.Width != 2000 || m.Filename != "wide-shot.jpg" || !strings.HasPrefix(m.StorageKey, "private/") {
		t.Fatalf("media row: %+v", m)
	}
	// Same bytes again: deduplicated.
	if _, dup, _ := svc.Upload(ctx, media.UploadInput{Filename: "copy.jpg", Body: bytes.NewReader(jb.Bytes())}); !dup {
		t.Fatal("duplicate not detected")
	}
	// Too many pixels.
	huge := image.NewGray(image.Rect(0, 0, 5000, 5000))
	var hb bytes.Buffer
	_ = png.Encode(&hb, huge)
	if _, _, err := svc.Upload(ctx, media.UploadInput{Filename: "huge.png", Body: bytes.NewReader(hb.Bytes())}); err == nil {
		t.Fatal("oversized image accepted")
	}
	// Variants.
	if err := svc.Process(context.Background(), jobs.MediaProcess{MediaID: m.ID.String()}); err != nil {
		t.Fatal(err)
	}
	views, _ := svc.Lookup(context.Background(), []uuid.UUID{m.ID})
	v := views[m.ID]
	if v == nil || !v.Ready || v.Variant("thumb").Width != 320 || v.Variant("large").Width != 1280 {
		t.Fatalf("variants: %+v", v)
	}
	if _, err := os.Stat(filepath.Join(dir, "public", "v", m.ID.String(), "large.jpg")); err != nil {
		t.Fatal("variant file missing")
	}
	if !strings.Contains(v.Srcset(), "1280w") {
		t.Fatalf("srcset: %s", v.Srcset())
	}
}
