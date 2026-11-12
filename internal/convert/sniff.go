package convert

import (
	"bytes"
	"io"
)

// sniffLen is the number of leading bytes Sniff peeks at. 12 covers every
// signature in the table below, including WebP's "RIFF"+size+"WEBP" (offset
// 8-11) and HEIC's "ftyp"+brand (offset 4-11).
//
// Audio formats (mp3/wav/m4a/ogg) are NOT registered here -- mp3's ID3v2 tag
// has a variable, declared length that can run well past this fixed window,
// so audio content is detected via the separate SniffAudio (audiosniff.go).
const sniffLen = 12

// heicBrands are the ISOBMFF major/compatible brands that identify HEIF/HEIC
// content. A "ftyp" box alone is not sufficient — MP4, MOV, and other ISOBMFF
// containers share the same box structure and must NOT be misdetected as heic.
var heicBrands = map[string]bool{
	"heic": true,
	"heix": true,
	"hevc": true,
	"hevx": true,
	"mif1": true,
	"msf1": true,
}

// signature pairs a registered format with a matcher over the peeked prefix.
type signature struct {
	format string
	match  func(buf []byte) bool
}

// signatures is the hardcoded, closed detection table (D-03). It covers the
// image formats registered in convert.Default (imageFormats in libvips.go:
// png, jpg, webp, heic, tiff) PLUS the fixed-offset video containers mp4,
// mov and avi.
//
// The video entries are deliberately live AHEAD of engine registration
// (WR-11, 34-REVIEW.md): AVConverter is built and tested in Phase 34 but not
// registered into convert.Default until Phase 35, so an mp4 upload is
// detected here and then rejected one step later at pair validation. mkv and
// webm are NOT in this table -- they need a variable-offset EBML walk
// (SniffVideo, avsniff.go) rather than a fixed 12-byte window, and that
// matcher is likewise not wired into the upload path until Phase 35. Both
// halves are fail-closed in the interim; keep this comment in sync when
// Phase 35 registers the engine.
var signatures = []signature{
	{"png", matchPNG},
	{"jpg", matchJPEG},
	{"webp", matchWebP},
	{"heic", matchHEIC},
	{"tiff", matchTIFF},
	{"mp4", matchMP4},
	{"mov", matchMOV},
	{"avi", matchAVI},
	{"pdf", matchPDF},
}

func matchPNG(b []byte) bool {
	sig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	return len(b) >= len(sig) && bytes.Equal(b[:len(sig)], sig)
}

func matchJPEG(b []byte) bool {
	sig := []byte{0xFF, 0xD8, 0xFF}
	return len(b) >= len(sig) && bytes.Equal(b[:len(sig)], sig)
}

func matchWebP(b []byte) bool {
	return len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
}

func matchTIFF(b []byte) bool {
	le := []byte{0x49, 0x49, 0x2A, 0x00}
	be := []byte{0x4D, 0x4D, 0x00, 0x2A}
	return len(b) >= 4 && (bytes.Equal(b[:4], le) || bytes.Equal(b[:4], be))
}

func matchHEIC(b []byte) bool {
	if len(b) < 12 || !bytes.Equal(b[4:8], []byte("ftyp")) {
		return false
	}
	return heicBrands[string(b[8:12])]
}

// pdfLeadingWhitespaceTolerance caps how many leading bytes matchPDF will
// skip before requiring the %PDF- magic (OCRP-01/D-01, 44-CONTEXT.md). The
// PDF spec (ISO 32000) only guarantees "%PDF-" appears near the start of the
// file -- some writers prepend a handful of whitespace/BOM-like bytes ahead
// of it -- but this is deliberately a SMALL, fixed tolerance (not the
// spec-permitted full 1024-byte scan window): sniffLen itself is only 12
// bytes, and a generous skip window would risk false-positiving arbitrary
// binary content that merely happens to contain "%PDF-" a few bytes in.
const pdfLeadingWhitespaceTolerance = 3

// matchPDF detects the PDF magic bytes "%PDF-" at offset 0, tolerating up to
// pdfLeadingWhitespaceTolerance leading whitespace bytes (space/tab/CR/LF/
// vtab/formfeed) ahead of it. PDF is the only genuinely new input content
// type this phase adds -- images are already sniffed by the matchers above.
func matchPDF(b []byte) bool {
	sig := []byte("%PDF-")
	i := 0
	for i < len(b) && i < pdfLeadingWhitespaceTolerance && isPDFLeadingWhitespace(b[i]) {
		i++
	}
	return len(b) >= i+len(sig) && bytes.Equal(b[i:i+len(sig)], sig)
}

func isPDFLeadingWhitespace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

// Sniff peeks at up to sniffLen bytes of r to identify the actual content
// format by magic bytes, independent of any filename/extension/Content-Type
// header. It returns the normalized detected format (via NormalizeFormat), or
// "" when no known signature matches (D-02 — unrecognized content). rest is
// an io.Reader that yields the FULL original stream (the peeked prefix
// re-stitched onto the remainder via io.MultiReader) so callers can still
// upload the intact file — Sniff never buffers the whole upload into memory
// (D-07). A short input (fewer than sniffLen bytes) is not an error: it
// simply fails to match some or all signatures.
func Sniff(r io.Reader) (detected string, rest io.Reader, err error) {
	buf := make([]byte, sniffLen)
	n, readErr := io.ReadFull(r, buf)
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		return "", nil, readErr
	}
	buf = buf[:n]
	rest = io.MultiReader(bytes.NewReader(buf), r)

	for _, sig := range signatures {
		if sig.match(buf) {
			return NormalizeFormat(sig.format), rest, nil
		}
	}
	return "", rest, nil
}

// MIMEType returns the canonical MIME type for a registered format, or
// "application/octet-stream" for anything unrecognized. This is the single
// home for format->MIME mapping shared by internal/api (stored Content-Type,
// D-06) and internal/worker (output Content-Type). Covers image formats
// (libvips), document formats + their pdf conversion target (LibreOffice),
// html + its pdf conversion target (chromium), the audio engine's four
// input formats plus its four transcription output targets (whisper,
// AUD-02), and the OCR engine's hocr/alto extraction targets (tesseract,
// OCRP-01 -- txt is already covered by the audio branch's shared "text/
// plain") -- so every job type is served with the same Content-Type
// correctness guarantee.
func MIMEType(format string) string {
	switch NormalizeFormat(format) {
	case "png":
		return "image/png"
	case "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "heic":
		return "image/heic"
	case "tiff":
		return "image/tiff"
	case "pdf":
		return "application/pdf"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	// The macro-free TEMPLATE types. Added by Phase 60: a sanitized .dotm/
	// .xltm/.potm/.ppsm carries its macro-free equivalent's content type
	// (D-06), so these are the extensions its output actually has. Without
	// them the object would be stored with an empty content type, which S3
	// serves as application/octet-stream and a browser offers as a download
	// of unknown kind.
	case "dotx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.template"
	case "xltx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.template"
	case "potx":
		return "application/vnd.openxmlformats-officedocument.presentationml.template"
	case "ppsx":
		return "application/vnd.openxmlformats-officedocument.presentationml.slideshow"
	case "odt":
		return "application/vnd.oasis.opendocument.text"
	case "ods":
		return "application/vnd.oasis.opendocument.spreadsheet"
	case "odp":
		return "application/vnd.oasis.opendocument.presentation"
	case "html":
		return "text/html"
	case "md":
		return "text/markdown"
	case "rst":
		return "text/x-rst"
	case "org":
		// org-mode has no registered IANA media type (PDCE-01, 47-CONTEXT.md)
		// -- stored and served as text/plain, the same honest-fallback
		// discipline this file already documents for hocr/alto above.
		return "text/plain"
	case "latex":
		return "application/x-latex"
	case "epub":
		return "application/epub+zip"
	case "step":
		return "model/step"
	case "iges":
		return "model/iges"
	case "stl":
		return "model/stl"
	case "svg":
		return "image/svg+xml"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "m4a":
		return "audio/mp4"
	case "ogg":
		return "audio/ogg"
	case "txt":
		return "text/plain"
	case "hocr":
		// hOCR is an HTML microformat (OCRP-01/D-01, 44-CONTEXT.md) -- stored
		// and served as text/html, not a distinct hocr-specific MIME type.
		return "text/html"
	case "alto":
		// ALTO is an XML schema (OCRP-01/D-01) -- stored and served as
		// application/xml.
		return "application/xml"
	case "srt":
		return "application/x-subrip"
	case "vtt":
		return "text/vtt"
	case "json":
		return "application/json"
	case "mp4":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	case "avi":
		return "video/x-msvideo"
	case "mkv":
		return "video/x-matroska"
	case "webm":
		return "video/webm"
	default:
		return "application/octet-stream"
	}
}
