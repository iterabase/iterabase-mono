package admission

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/adler32"
	"hash/crc32"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
)

// Adversarial fixtures specific to this package's structural checks. The
// shared, importable corpus lives in admissiontest.

func zlibBytes(t testing.TB, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	_, err := w.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return out.Bytes()
}

// flateStream returns a FlateDecode stream object holding content.
func flateStream(t testing.TB, content string) string {
	return flateStreamRaw(zlibBytes(t, []byte(content)))
}

func flateStreamRaw(compressed []byte) string {
	return fmt.Sprintf("<< /Type /ObjStm /N 1 /First 4 /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream",
		len(compressed), compressed)
}

// zeroDeflate returns a raw deflate stream of mib MiB of zeros, built by
// repeating one flushed (non-final) 1 MiB block, so a large bomb is cheap to
// generate. crc and adler are the checksums of the inflated bytes.
func zeroDeflate(t testing.TB, mib int) (raw []byte, crc, adler uint32) {
	t.Helper()
	zeros := make([]byte, 1<<20)
	var chunk bytes.Buffer
	w, err := flate.NewWriter(&chunk, flate.BestCompression)
	require.NoError(t, err)
	_, err = w.Write(zeros)
	require.NoError(t, err)
	require.NoError(t, w.Flush())
	crcHash, adlerHash := crc32.NewIEEE(), adler32.New()
	var out bytes.Buffer
	for range mib {
		out.Write(chunk.Bytes())
		_, _ = crcHash.Write(zeros)
		_, _ = adlerHash.Write(zeros)
	}
	out.Write([]byte{0x03, 0x00}) // final empty fixed-Huffman block
	return out.Bytes(), crcHash.Sum32(), adlerHash.Sum32()
}

// zlibZeroBomb is a zlib stream inflating to mib MiB of zeros.
func zlibZeroBomb(t testing.TB, mib int) []byte {
	raw, _, adler := zeroDeflate(t, mib)
	out := append([]byte{0x78, 0xDA}, raw...)
	return binary.BigEndian.AppendUint32(out, adler)
}

// rawZipEntry is a deflated ZIP entry whose header declares declared bytes
// while its data really inflates to mib MiB of zeros.
type rawZipEntry struct {
	name     string
	declared uint64
	mib      int
}

func zipWithRaw(t testing.TB, entries []admissiontest.ZipEntry, raws ...rawZipEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, e := range entries {
		f, err := w.Create(e.Name)
		require.NoError(t, err)
		_, err = f.Write(e.Data)
		require.NoError(t, err)
	}
	for _, r := range raws {
		data, crc, _ := zeroDeflate(t, r.mib)
		f, err := w.CreateRaw(&zip.FileHeader{
			Name: r.name, Method: zip.Deflate, CRC32: crc,
			CompressedSize64: uint64(len(data)), UncompressedSize64: r.declared,
		})
		require.NoError(t, err)
		_, err = f.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return out.Bytes()
}

func docxWithDocumentRels(rel string) []byte {
	return admissiontest.DOCXWith(admissiontest.ZipEntry{Name: "word/_rels/document.xml.rels", Data: []byte(admissiontest.RelsXML(rel))})
}

// pngHeaderOnly is a PNG whose IHDR claims width x height; it has no data.
func pngHeaderOnly(width, height uint32) []byte {
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], width)
	binary.BigEndian.PutUint32(ihdr[4:], height)
	ihdr[8], ihdr[9] = 8, 2
	writePNGChunk(&out, "IHDR", ihdr)
	writePNGChunk(&out, "IEND", nil)
	return out.Bytes()
}

func writePNGChunk(out *bytes.Buffer, kind string, data []byte) {
	_ = binary.Write(out, binary.BigEndian, uint32(len(data)))
	chunk := append([]byte(kind), data...)
	out.Write(chunk)
	_ = binary.Write(out, binary.BigEndian, crc32.ChecksumIEEE(chunk))
}

// tiffChain is a little-endian TIFF of pages 1x1 IFDs chained together.
func tiffChain(pages int) []byte {
	out := []byte("II*\x00\x08\x00\x00\x00")
	for page := range pages {
		ifd := make([]byte, 2+2*12+4)
		binary.LittleEndian.PutUint16(ifd, 2)
		for i, tag := range []uint16{tiffTagImageWidth, tiffTagImageLength} {
			entry := ifd[2+i*12:]
			binary.LittleEndian.PutUint16(entry, tag)
			binary.LittleEndian.PutUint16(entry[2:], tiffTypeShort)
			binary.LittleEndian.PutUint32(entry[4:], 1)
			binary.LittleEndian.PutUint16(entry[8:], 1)
		}
		next := uint32(0)
		if page < pages-1 {
			next = uint32(len(out) + len(ifd))
		}
		binary.LittleEndian.PutUint32(ifd[len(ifd)-4:], next)
		out = append(out, ifd...)
	}
	return out
}
