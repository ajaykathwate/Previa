package auth

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodedImage(t *testing.T, width, height int, fill func(x, y int) color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, fill(x, y))
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestNormalizeAvatarCropsScalesAndStripsMetadata(t *testing.T) {
	// A wide image keeps only its centred square: red bars either side are cropped.
	wide := encodedImage(t, 600, 300, func(x, _ int) color.Color {
		if x < 150 || x >= 450 {
			return color.NRGBA{R: 255, A: 255}
		}
		return color.NRGBA{G: 200, B: 40, A: 255}
	})
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 40, 30)), nil); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		data []byte
		side int
	}{
		{"wide PNG is cropped and scaled", append(wide, []byte("trailing metadata")...), 256},
		{"small JPEG keeps its size", jpg.Bytes(), 30},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeAvatar(tt.data)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(got, []byte("metadata")) {
				t.Fatal("retained trailing metadata")
			}
			img, format, err := image.Decode(bytes.NewReader(got))
			if err != nil || format != "png" || img.Bounds().Dx() != tt.side || img.Bounds().Dy() != tt.side {
				t.Fatalf("normalized avatar: %v %s %v", img.Bounds(), format, err)
			}
			if tt.side == 256 {
				for _, x := range []int{0, 128, 255} {
					if r, g, _, _ := img.At(x, 128).RGBA(); r>>8 > 10 || g>>8 < 190 {
						t.Fatalf("pixel %d kept cropped colour: r=%d g=%d", x, r>>8, g>>8)
					}
				}
			}
		})
	}
	for name, data := range map[string][]byte{
		"empty":     nil,
		"svg":       []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`),
		"truncated": wide[:40],
		"too large": make([]byte, MaxAvatarBytes+1),
		"too wide":  encodedImage(t, maxAvatarSide+1, 1, func(int, int) color.Color { return color.White }),
	} {
		if _, err := normalizeAvatar(data); !errors.Is(err, ErrAvatar) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

func TestShrinkAveragesBlocks(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			v := uint8(0)
			if (x+y)%2 == 0 {
				v = 200
			}
			src.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	got := shrink(src, 2)
	for y := range 2 {
		for x := range 2 {
			if c := got.RGBAAt(x, y); c.R != 100 || c.A != 255 {
				t.Fatalf("block (%d,%d) = %+v, want an even average", x, y, c)
			}
		}
	}
}
