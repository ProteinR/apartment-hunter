package dedup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"strings"
)

type ImageHasher struct {
	client *http.Client
}

func NewImageHasher() ImageHasher {
	return ImageHasher{client: http.DefaultClient}
}

func NewImageHasherWithClient(client *http.Client) ImageHasher {
	if client == nil {
		client = http.DefaultClient
	}
	return ImageHasher{client: client}
}

func (h ImageHasher) HashURL(ctx context.Context, imageURL string) (ImageHashes, error) {
	data, err := h.readBytes(ctx, imageURL)
	if err != nil {
		return ImageHashes{}, err
	}
	return h.HashBytes(data)
}

func (h ImageHasher) HashBytes(data []byte) (ImageHashes, error) {
	raw := sha256.Sum256(data)
	hashes := ImageHashes{SHA256: hex.EncodeToString(raw[:])}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return hashes, fmt.Errorf("decode image: %w", err)
	}

	hashes.PHashFull = formatHash(averageHash(img, img.Bounds()))
	hashes.PHashCenter80 = formatHash(averageHash(img, centerCrop(img.Bounds(), 0.80)))
	hashes.PHashCenter60 = formatHash(averageHash(img, centerCrop(img.Bounds(), 0.60)))
	hashes.DHashFull = formatHash(differenceHash(img, img.Bounds()))
	return hashes, nil
}

func (h ImageHasher) readBytes(ctx context.Context, imageURL string) ([]byte, error) {
	switch {
	case strings.HasPrefix(imageURL, "http://"), strings.HasPrefix(imageURL, "https://"):
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := h.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("download image %s: HTTP %d", imageURL, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	case strings.HasPrefix(imageURL, "file://"):
		return os.ReadFile(strings.TrimPrefix(imageURL, "file://"))
	default:
		return os.ReadFile(imageURL)
	}
}

func averageHash(img image.Image, bounds image.Rectangle) uint64 {
	var samples [64]uint8
	sum := 0
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			px := bounds.Min.X + (x*bounds.Dx()+bounds.Dx()/2)/8
			py := bounds.Min.Y + (y*bounds.Dy()+bounds.Dy()/2)/8
			value := grayscale(img.At(px, py))
			samples[y*8+x] = value
			sum += int(value)
		}
	}
	avg := sum / len(samples)
	var hash uint64
	for i, sample := range samples {
		if int(sample) >= avg {
			hash |= 1 << uint(63-i)
		}
	}
	return hash
}

func differenceHash(img image.Image, bounds image.Rectangle) uint64 {
	var hash uint64
	bit := 0
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			leftX := bounds.Min.X + (x*bounds.Dx()+bounds.Dx()/2)/9
			rightX := bounds.Min.X + ((x+1)*bounds.Dx()+bounds.Dx()/2)/9
			py := bounds.Min.Y + (y*bounds.Dy()+bounds.Dy()/2)/8
			if grayscale(img.At(leftX, py)) > grayscale(img.At(rightX, py)) {
				hash |= 1 << uint(63-bit)
			}
			bit++
		}
	}
	return hash
}

func centerCrop(bounds image.Rectangle, ratio float64) image.Rectangle {
	width := int(float64(bounds.Dx()) * ratio)
	height := int(float64(bounds.Dy()) * ratio)
	x0 := bounds.Min.X + (bounds.Dx()-width)/2
	y0 := bounds.Min.Y + (bounds.Dy()-height)/2
	return image.Rect(x0, y0, x0+width, y0+height)
}

func grayscale(c color.Color) uint8 {
	r, g, b, _ := c.RGBA()
	return uint8((299*r + 587*g + 114*b) / 1000 / 256)
}

func formatHash(hash uint64) string {
	return fmt.Sprintf("%016x", hash)
}

func HammingDistanceHex(left, right string) (int, error) {
	leftBytes, err := hex.DecodeString(left)
	if err != nil {
		return 0, err
	}
	rightBytes, err := hex.DecodeString(right)
	if err != nil {
		return 0, err
	}
	if len(leftBytes) != len(rightBytes) {
		return 0, fmt.Errorf("hash lengths differ")
	}

	distance := 0
	for i := range leftBytes {
		distance += bitsInByte(leftBytes[i] ^ rightBytes[i])
	}
	return distance, nil
}

func bitsInByte(value byte) int {
	count := 0
	for value > 0 {
		count += int(value & 1)
		value >>= 1
	}
	return count
}
