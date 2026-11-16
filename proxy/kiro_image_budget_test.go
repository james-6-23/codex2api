package proxy

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestKiroDecodeBudgetTransparentGray16PNG(t *testing.T) {
	var source bytes.Buffer
	source.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		t.Helper()
		_ = binary.Write(&source, binary.BigEndian, uint32(len(data)))
		payload := append([]byte(kind), data...)
		source.Write(payload)
		_ = binary.Write(&source, binary.BigEndian, crc32.ChecksumIEEE(payload))
	}
	chunk("IHDR", []byte{0, 0, 0, 1, 0, 0, 0, 1, 16, 0, 0, 0, 0})
	chunk("tRNS", []byte{0, 0})
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, _ = writer.Write([]byte{0, 0, 0})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	chunk("IDAT", compressed.Bytes())
	chunk("IEND", nil)
	config, err := png.DecodeConfig(bytes.NewReader(source.Bytes()))
	if err != nil || config.ColorModel != color.Gray16Model {
		t.Fatalf("header: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(source.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.(*image.NRGBA64); !ok {
		t.Fatalf("actual decoded type: %T", decoded)
	}
	raw := source.Bytes()
	binary.BigEndian.PutUint32(raw[16:20], 10000)
	binary.BigEndian.PutUint32(raw[20:24], 10000)
	binary.BigEndian.PutUint32(raw[29:33], crc32.ChecksumIEEE(raw[12:29]))
	config, err = png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || kiroImageDecodeWithinBudget(config.Width, config.Height) {
		t.Fatal("tRNS expansion must be rejected before decoding")
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if _, result := kiroShrinkImage("png", encoded); result != encoded {
		t.Fatal("oversized image must remain untouched")
	}
}

func TestKiroDecodeBudgetProgressiveJPEG(t *testing.T) {
	header := []byte{0xff, 0xd8, 0xff, 0xc2, 0, 11, 8, 0x4e, 0x20, 0x4e, 0x20, 1, 1, 0x11, 0, 0xff, 0xda, 0, 8, 1, 1, 0, 0, 0, 0}
	config, err := jpeg.DecodeConfig(bytes.NewReader(header))
	if err != nil || config.Width != 20000 || config.ColorModel != color.GrayModel {
		t.Fatalf("progressive fixture: %+v %v", config, err)
	}
	if kiroImageDecodeWithinBudget(config.Width, config.Height) {
		t.Fatal("progressive JPEG coefficient memory must be budgeted")
	}
	encoded := base64.StdEncoding.EncodeToString(header)
	if _, result := kiroShrinkImage("jpeg", encoded); result != encoded {
		t.Fatal("oversized image must remain untouched")
	}
}
