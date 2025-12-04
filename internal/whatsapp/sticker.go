package whatsapp

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
)

// StickerMetadata captures pack info stored in EXIF UserComment for stickers.
type StickerMetadata struct {
	PackID      string
	PackName    string
	PackPub     string
	AndroidLink string
	IOSLink     string
	Emojis      []string
}

func (m StickerMetadata) normalizeDefaults() StickerMetadata {
	out := m
	if strings.TrimSpace(out.PackName) == "" {
		out.PackName = "WhatsApp Bot"
	}
	if strings.TrimSpace(out.PackPub) == "" {
		out.PackPub = "karisma.id"
	}
	if strings.TrimSpace(out.PackID) == "" {
		out.PackID = "bot-pack"
	}
	if out.Emojis == nil {
		out.Emojis = []string{}
	}
	return out
}

func defaultStickerMeta() StickerMetadata {
	return StickerMetadata{
		PackID:   "bot-pack",
		PackName: "WhatsApp Bot",
		PackPub:  "karisma.id",
	}
}

func sniffWebP(data []byte) (isWebP bool, isAnimated bool) {
	if len(data) < 12 {
		return false, false
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return false, false
	}

	pos := 12
	for pos+8 <= len(data) {
		chunk := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		if chunk == "ANIM" {
			return true, true
		}
		pos += 8 + size
		if pos%2 == 1 {
			pos++
		}
	}
	return true, false
}

func isZip(data []byte) bool {
	return len(data) >= 4 && string(data[:4]) == "PK\x03\x04"
}

func injectStickerMetadataEXIF(data []byte, meta StickerMetadata) ([]byte, error) {
	if ok, _ := sniffWebP(data); !ok {
		return nil, errors.New("sticker payload is not valid WebP")
	}

	meta = meta.normalizeDefaults()

	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil, errors.New("invalid webp header")
	}

	chunks, err := parseWebPChunks(data)
	if err != nil {
		return nil, err
	}

	exifPayload, err := buildStickerExifPayload(meta)
	if err != nil {
		return nil, err
	}

	exifChunk := encodeChunk("EXIF", exifPayload)

	width, height := canvasFromChunks(chunks)
	vp8xFlags, hasVP8X := existingVP8XFlags(chunks)
	vp8xFlags |= 0x04 // EXIF present flag per WebP spec (bit 2)
	vp8xChunk := buildVP8XChunk(width, height, vp8xFlags, hasVP8X)

	var rebuiltChunks [][]byte
	inserted := false
	for _, ch := range chunks {
		if ch.fourCC == "VP8X" {
			rebuiltChunks = append(rebuiltChunks, vp8xChunk)
			inserted = true
			continue
		}
		if ch.fourCC == "EXIF" {
			continue
		}
		rebuiltChunks = append(rebuiltChunks, encodeChunk(ch.fourCC, ch.payload))
	}
	if !inserted {
		rebuiltChunks = append([][]byte{vp8xChunk}, rebuiltChunks...)
	}
	rebuiltChunks = append(rebuiltChunks, exifChunk)

	totalSize := 4 // "WEBP"
	for _, c := range rebuiltChunks {
		totalSize += len(c)
	}
	riffSize := uint32(totalSize)

	buf := bytes.NewBuffer(make([]byte, 0, totalSize+8))
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, riffSize)
	buf.WriteString("WEBP")
	for _, c := range rebuiltChunks {
		buf.Write(c)
	}
	return buf.Bytes(), nil
}

func buildStickerExifPayload(meta StickerMetadata) ([]byte, error) {
	jsonPayload, err := buildStickerMetadataText(meta)
	if err != nil {
		return nil, err
	}

	// Matches the wa-sticker-formatter layout:
	// TIFF little-endian header, one IFD entry (tag 0x5741, type 7/undefined),
	// count = JSON length, value offset = 0x16, then JSON bytes.
	header := []byte{
		0x49, 0x49, 0x2a, 0x00, // TIFF little-endian
		0x08, 0x00, 0x00, 0x00, // offset to IFD (8)
		0x01, 0x00, // number of entries (1)
		0x41, 0x57, // tag 0x5741 ("AW")
		0x07, 0x00, // type = UNDEFINED (7)
		0x00, 0x00, 0x00, 0x00, // count (filled below)
		0x16, 0x00, 0x00, 0x00, // value offset (22)
	}
	binary.LittleEndian.PutUint32(header[14:], uint32(len(jsonPayload)))

	exifBuf := make([]byte, 0, len(header)+len(jsonPayload))
	exifBuf = append(exifBuf, header...)
	exifBuf = append(exifBuf, jsonPayload...)
	return exifBuf, nil
}

func buildStickerMetadataText(meta StickerMetadata) ([]byte, error) {
	meta = meta.normalizeDefaults()
	payload := map[string]interface{}{
		"sticker-pack-id":        meta.PackID,
		"sticker-pack-name":      meta.PackName,
		"sticker-pack-publisher": meta.PackPub,
		"emojis":                 meta.Emojis,
	}

	return json.Marshal(payload)
}

type webpChunk struct {
	fourCC  string
	payload []byte
}

func parseWebPChunks(data []byte) ([]webpChunk, error) {
	if len(data) < 12 {
		return nil, errors.New("webp too small")
	}
	pos := 12
	var chunks []webpChunk
	for pos+8 <= len(data) {
		fourCC := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		if pos+8+size > len(data) {
			return nil, errors.New("invalid webp chunk size")
		}
		payload := data[pos+8 : pos+8+size]
		chunks = append(chunks, webpChunk{fourCC: fourCC, payload: payload})
		pos += 8 + size
		if pos%2 == 1 {
			pos++
		}
	}
	return chunks, nil
}

func encodeChunk(fourCC string, payload []byte) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len(payload)+12))
	buf.WriteString(fourCC)
	_ = binary.Write(buf, binary.LittleEndian, uint32(len(payload)))
	buf.Write(payload)
	if len(payload)%2 == 1 {
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

func existingVP8XFlags(chunks []webpChunk) (byte, bool) {
	for _, ch := range chunks {
		if ch.fourCC != "VP8X" || len(ch.payload) < 1 {
			continue
		}
		return ch.payload[0], true
	}
	return 0, false
}

func canvasFromChunks(chunks []webpChunk) (width, height uint32) {
	for _, ch := range chunks {
		switch ch.fourCC {
		case "VP8X":
			if len(ch.payload) >= 10 {
				wm1 := uint32(ch.payload[4]) | uint32(ch.payload[5])<<8 | uint32(ch.payload[6])<<16
				hm1 := uint32(ch.payload[7]) | uint32(ch.payload[8])<<8 | uint32(ch.payload[9])<<16
				return wm1 + 1, hm1 + 1
			}
		case "VP8 ":
			if len(ch.payload) >= 10 && ch.payload[3] == 0x9d && ch.payload[4] == 0x01 && ch.payload[5] == 0x2a {
				w := binary.LittleEndian.Uint16(ch.payload[6:8]) & 0x3FFF
				h := binary.LittleEndian.Uint16(ch.payload[8:10]) & 0x3FFF
				if w > 0 && h > 0 {
					return uint32(w), uint32(h)
				}
			}
		case "VP8L":
			if len(ch.payload) >= 5 && ch.payload[0] == 0x2f {
				b1 := ch.payload[1]
				b2 := ch.payload[2]
				b3 := ch.payload[3]
				b4 := ch.payload[4]
				w := 1 + uint32(b1) + (uint32(b2&0x3F) << 8)
				h := 1 + (uint32(b2>>6) + (uint32(b3) << 2) + (uint32(b4&0x0F) << 10))
				if w > 0 && h > 0 {
					return w, h
				}
			}
		}
	}
	return 512, 512 // fallback
}

func buildVP8XChunk(width, height uint32, flags byte, hadVP8X bool) []byte {
	if width == 0 {
		width = 512
	}
	if height == 0 {
		height = 512
	}
	if width > 0xFFFFFF {
		width = 0xFFFFFF
	}
	if height > 0xFFFFFF {
		height = 0xFFFFFF
	}

	payload := make([]byte, 10)
	payload[0] = flags
	// bytes 1-3 reserved (zero)
	wm1 := width - 1
	hm1 := height - 1
	payload[4] = byte(wm1 & 0xFF)
	payload[5] = byte((wm1 >> 8) & 0xFF)
	payload[6] = byte((wm1 >> 16) & 0xFF)
	payload[7] = byte(hm1 & 0xFF)
	payload[8] = byte((hm1 >> 8) & 0xFF)
	payload[9] = byte((hm1 >> 16) & 0xFF)

	return encodeChunk("VP8X", payload)
}
