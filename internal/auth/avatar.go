package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // Accept JPEG uploads; stored pictures are re-encoded PNGs.
	"image/png"
	"strconv"

	"github.com/ajaykathwatee/previa/internal/auth/dbgen"
)

const (
	// MaxAvatarBytes fits the dashboard's form cap. The browser shrinks larger
	// photos to avatarSize before upload; without JavaScript this limit applies.
	MaxAvatarBytes  = 192 << 10
	maxAvatarSide   = 2048 // bounds decode memory for untrusted images
	avatarSize      = 256
	avatarCacheTime = "private, max-age=3600"
)

var ErrAvatar = errors.New("choose a PNG or JPEG image up to 192 KiB and 2048 × 2048 pixels")

// AvatarSrc is the picture shown for the member: an uploaded photo's versioned
// address, otherwise their GitHub picture.
func (u User) AvatarSrc() string {
	if u.CustomAvatar && u.PublicID != "" {
		return "/avatars/" + u.PublicID + "?v=" + strconv.FormatInt(u.AvatarVersion, 10)
	}
	return u.AvatarURL
}

func (r *Repository) SaveAvatar(ctx context.Context, user int64, upload []byte) (int64, error) {
	data, err := normalizeAvatar(upload)
	if err != nil {
		return 0, err
	}
	version, err := r.queries.SaveAvatar(ctx, dbgen.SaveAvatarParams{UserID: user, Data: data})
	if err != nil {
		return 0, fmt.Errorf("save profile picture: %w", err)
	}
	return version, nil
}

func (r *Repository) RemoveAvatar(ctx context.Context, user int64) error {
	if err := r.queries.RemoveAvatar(ctx, user); err != nil {
		return fmt.Errorf("remove profile picture: %w", err)
	}
	return nil
}

func (r *Repository) Avatar(ctx context.Context, user int64) ([]byte, int64, error) {
	v, err := r.queries.ReadAvatar(ctx, user)
	if err != nil {
		return nil, 0, fmt.Errorf("read profile picture: %w", err)
	}
	return v.Data, v.AvatarVersion, nil
}

// normalizeAvatar decodes a bounded PNG or JPEG, crops it to a centred square,
// scales it down to avatarSize and re-encodes it as PNG, which drops metadata.
func normalizeAvatar(upload []byte) ([]byte, error) {
	if len(upload) == 0 || len(upload) > MaxAvatarBytes {
		return nil, ErrAvatar
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(upload))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxAvatarSide || cfg.Height > maxAvatarSide {
		return nil, ErrAvatar
	}
	decoded, _, err := image.Decode(bytes.NewReader(upload))
	if err != nil {
		return nil, ErrAvatar
	}
	b := decoded.Bounds()
	side := min(b.Dx(), b.Dy())
	crop := image.Rect(0, 0, side, side)
	square := image.NewRGBA(crop)
	draw.Draw(square, crop, decoded, image.Pt(b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2), draw.Src)
	var out bytes.Buffer
	if err := png.Encode(&out, shrink(square, min(side, avatarSize))); err != nil {
		return nil, fmt.Errorf("encode profile picture: %w", err)
	}
	return out.Bytes(), nil
}

// shrink area-averages a square RGBA image to size × size. Averaging the
// premultiplied channels keeps transparent edges correct.
func shrink(src *image.RGBA, size int) *image.RGBA {
	side := src.Bounds().Dx()
	if size >= side {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		y0, y1 := y*side/size, max((y+1)*side/size, y*side/size+1)
		for x := range size {
			x0, x1 := x*side/size, max((x+1)*side/size, x*side/size+1)
			var sum [4]int
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride+x0*4 : sy*src.Stride+x1*4]
				for i := 0; i < len(row); i += 4 {
					sum[0] += int(row[i])
					sum[1] += int(row[i+1])
					sum[2] += int(row[i+2])
					sum[3] += int(row[i+3])
				}
			}
			n := (y1 - y0) * (x1 - x0)
			o := y*dst.Stride + x*4
			for c := range 4 {
				dst.Pix[o+c] = uint8((sum[c] + n/2) / n)
			}
		}
	}
	return dst
}
