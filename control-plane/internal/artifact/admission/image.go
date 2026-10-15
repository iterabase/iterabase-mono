package admission

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/tiff"
)

const (
	tiffTagImageWidth  = 256
	tiffTagImageLength = 257
	tiffTypeShort      = 3
	tiffTypeLong       = 4
)

// validateImage checks dimensions from the header before a complete decode,
// so the decode allocation is bounded by MaxPixelsPerImage.
func validateImage(data []byte, canonical string) string {
	var (
		decodeConfig func(io.Reader) (image.Config, error)
		decode       func(io.Reader) (image.Image, error)
	)
	switch canonical {
	case MIMEJPEG:
		decodeConfig, decode = jpeg.DecodeConfig, jpeg.Decode
	case MIMEPNG:
		decodeConfig, decode = png.DecodeConfig, png.Decode
	case MIMETIFF:
		if reason := validateTIFFPages(data); reason != "" {
			return reason
		}
		decodeConfig, decode = tiff.DecodeConfig, tiff.Decode
	default:
		return ReasonUnsupportedType
	}
	config, err := decodeConfig(bytes.NewReader(data))
	if err != nil {
		return ReasonMalformed
	}
	if reason := pixelReason(int64(config.Width), int64(config.Height)); reason != "" {
		return reason
	}
	if _, err := decode(bytes.NewReader(data)); err != nil {
		return ReasonMalformed
	}
	return ""
}

func pixelReason(width, height int64) string {
	if width <= 0 || height <= 0 {
		return ReasonMalformed
	}
	if width > MaxPixelsPerImage || height > MaxPixelsPerImage || width*height > MaxPixelsPerImage {
		return ReasonImageTooLarge
	}
	return ""
}

// validateTIFFPages walks the IFD chain structurally: at most MaxTIFFPages
// pages, no cycles, each page within MaxPixelsPerImage, and the aggregate
// within MaxAggregatePixels. Only the first page is fully decoded afterwards.
func validateTIFFPages(data []byte) string {
	if len(data) < 8 {
		return ReasonMalformed
	}
	var order binary.ByteOrder = binary.LittleEndian
	if data[0] == 'M' {
		order = binary.BigEndian
	}
	offset := int64(order.Uint32(data[4:8]))
	visited := make(map[int64]bool)
	var total int64
	for pages := 0; offset != 0; pages++ {
		if pages == MaxTIFFPages {
			return ReasonResourceLimit
		}
		if visited[offset] {
			return ReasonMalformed
		}
		visited[offset] = true
		width, height, next, ok := readIFD(data, offset, order)
		if !ok {
			return ReasonMalformed
		}
		if reason := pixelReason(width, height); reason != "" {
			return reason
		}
		total += width * height
		if total > MaxAggregatePixels {
			return ReasonResourceLimit
		}
		offset = next
	}
	return ""
}

// readIFD reads one IFD's image dimensions and the offset of the next IFD.
func readIFD(data []byte, offset int64, order binary.ByteOrder) (width, height, next int64, ok bool) {
	size := int64(len(data))
	if offset < 8 || offset+2 > size {
		return 0, 0, 0, false
	}
	count := int64(order.Uint16(data[offset:]))
	end := offset + 2 + count*12
	if end+4 > size {
		return 0, 0, 0, false
	}
	for entry := offset + 2; entry < end; entry += 12 {
		tag := order.Uint16(data[entry:])
		var value int64
		switch order.Uint16(data[entry+2:]) {
		case tiffTypeShort:
			value = int64(order.Uint16(data[entry+8:]))
		case tiffTypeLong:
			value = int64(order.Uint32(data[entry+8:]))
		default:
			continue
		}
		switch tag {
		case tiffTagImageWidth:
			width = value
		case tiffTagImageLength:
			height = value
		}
	}
	return width, height, int64(order.Uint32(data[end:])), true
}
