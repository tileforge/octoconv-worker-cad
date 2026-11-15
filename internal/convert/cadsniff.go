package convert

import (
	"bytes"
	"encoding/binary"
	"io"
	"strconv"
)

type CADRejection string

const (
	CADRejectNone         CADRejection = ""
	CADRejectTruncated    CADRejection = "truncated"
	CADRejectAmbiguousSTL CADRejection = "ambiguous_stl"
	CADRejectBinaryDXF    CADRejection = "binary_dxf"
)

func AllCADRejections() []CADRejection {
	return []CADRejection{CADRejectTruncated, CADRejectAmbiguousSTL, CADRejectBinaryDXF}
}

type CADResult struct {
	Format    string
	Rejection CADRejection
}

const (
	CADHeadBytes = 4096
	CADTailBytes = 4096
)

var (
	stepMagic     = []byte("ISO-10303-21;")
	stepHeaderTag = []byte("HEADER;")
	stepEndTag    = []byte("END-ISO-10303-21;")
	dwgPrefix     = []byte("AC10")
	binaryDXFTag  = []byte("AutoCAD Binary DXF\r\n\x1a\x00")
	asciiSolid    = []byte("solid")
	asciiEndSolid = []byte("endsolid")
)

func IsCADDeclared(format string) bool {
	switch format {
	case "step", "iges", "stl", "dxf", "dwg":
		return true
	}
	return false
}

func ReadCADTail(r io.ReaderAt, size int64) ([]byte, error) {
	if size <= 0 {
		return nil, nil
	}
	offset := size - CADTailBytes
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, size-offset)
	n, err := r.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

func ReadCADWindows(r io.ReaderAt, size int64) (head, tail []byte, err error) {
	if size <= 0 {
		return nil, nil, nil
	}
	n := size
	if n > CADHeadBytes {
		n = CADHeadBytes
	}
	buf := make([]byte, n)
	read, err := r.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	tail, err = ReadCADTail(r, size)
	if err != nil {
		return nil, nil, err
	}
	return buf[:read], tail, nil
}

func isASCIISpace(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', '\f', '\v':
		return true
	}
	return false
}

func trimLeftASCIISpace(b []byte) []byte {
	i := 0
	for i < len(b) && isASCIISpace(b[i]) {
		i++
	}
	return b[i:]
}

func trimRightASCIISpace(b []byte) []byte {
	n := len(b)
	for n > 0 && isASCIISpace(b[n-1]) {
		n--
	}
	return b[:n]
}

func SniffCADWindows(head, tail []byte, size int64, declared string) CADResult {
	if declared == "stl" && binarySTLSize(head, size) {
		return sniffSTL(head, tail, size)
	}
	if len(head) >= 6 && bytes.HasPrefix(head, dwgPrefix) && isASCIIDigits(head[4:6]) {
		return CADResult{Format: "dwg"}
	}
	if bytes.HasPrefix(head, binaryDXFTag) {
		return CADResult{Format: "dxf", Rejection: CADRejectBinaryDXF}
	}
	if bytes.HasPrefix(trimLeftASCIISpace(head), stepMagic) {
		if !bytes.Contains(head, stepHeaderTag) || !bytes.HasSuffix(trimRightASCIISpace(tail), stepEndTag) {
			return CADResult{Format: "step", Rejection: CADRejectTruncated}
		}
		return CADResult{Format: "step"}
	}
	if res, ok := sniffIGES(head, tail, size); ok {
		return res
	}
	if res, ok := sniffDXF(head, tail); ok {
		return res
	}
	if declared == "stl" {
		return sniffSTL(head, tail, size)
	}
	return CADResult{}
}

func isASCIIDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(b) > 0
}

func parseIGESField(b []byte) int64 {
	v, err := strconv.ParseInt(string(bytes.TrimSpace(b)), 10, 64)
	if err != nil {
		return -1
	}
	return v
}

func sniffIGES(head, tail []byte, size int64) (CADResult, bool) {
	if len(head) < 80 || head[72] != 'S' || parseIGESField(head[73:80]) != 1 {
		return CADResult{}, false
	}
	truncated := CADResult{Format: "iges", Rejection: CADRejectTruncated}
	stride := int64(80)
	switch {
	case len(head) > 80 && head[80] == '\n':
		stride = 81
	case len(head) > 81 && head[80] == '\r' && head[81] == '\n':
		stride = 82
	}
	trimmed := trimRightASCIISpace(tail)
	size -= int64(len(tail) - len(trimmed))
	if len(trimmed) < 80 {
		return truncated, true
	}
	rec := trimmed[len(trimmed)-80:]
	if rec[72] != 'T' || rec[0] != 'S' || rec[8] != 'G' || rec[16] != 'D' || rec[24] != 'P' {
		return truncated, true
	}
	s, g, d, p := parseIGESField(rec[1:8]), parseIGESField(rec[9:16]), parseIGESField(rec[17:24]), parseIGESField(rec[25:32])
	if s < 0 || g < 0 || d < 0 || p < 0 {
		return truncated, true
	}
	fit := size + stride - 80
	if fit%stride != 0 || fit/stride != s+g+d+p+1 {
		return truncated, true
	}
	return CADResult{Format: "iges"}, true
}

func sniffDXF(head, tail []byte) (CADResult, bool) {
	lines := bytes.Split(head, []byte("\n"))
	pairs := len(lines) / 2
	found := false
	for i := 0; i < pairs; i++ {
		code := bytes.TrimSpace(lines[2*i])
		value := bytes.TrimSpace(lines[2*i+1])
		if string(code) == "999" {
			continue
		}
		if string(code) == "0" && string(value) == "SECTION" {
			found = true
		}
		break
	}
	if !found {
		return CADResult{}, false
	}
	truncated := CADResult{Format: "dxf", Rejection: CADRejectTruncated}
	t := trimRightASCIISpace(tail)
	cut := bytes.LastIndexByte(t, '\n')
	if cut < 0 || string(bytes.TrimSpace(t[cut+1:])) != "EOF" {
		return truncated, true
	}
	prev := t[:cut]
	if string(bytes.TrimSpace(prev[bytes.LastIndexByte(prev, '\n')+1:])) != "0" {
		return truncated, true
	}
	return CADResult{Format: "dxf"}, true
}

func binarySTLSize(head []byte, size int64) bool {
	if size < 84 || len(head) < 84 {
		return false
	}
	return size == 84+50*int64(binary.LittleEndian.Uint32(head[80:84]))
}

func hasFoldedPrefix(b, prefix []byte) bool {
	return len(b) >= len(prefix) && bytes.EqualFold(b[:len(prefix)], prefix)
}

func sniffSTL(head, tail []byte, size int64) CADResult {
	isBinary := binarySTLSize(head, size)
	asciiHead := hasFoldedPrefix(trimLeftASCIISpace(head), asciiSolid)
	t := trimRightASCIISpace(tail)
	last := bytes.TrimLeft(t[bytes.LastIndexByte(t, '\n')+1:], " \t")
	asciiTail := hasFoldedPrefix(last, asciiEndSolid) && bytes.IndexByte(last, 0) < 0
	switch {
	case isBinary && asciiHead && asciiTail:
		return CADResult{Format: "stl", Rejection: CADRejectAmbiguousSTL}
	case isBinary, asciiHead && asciiTail:
		return CADResult{Format: "stl"}
	case size >= 84 || asciiHead:
		return CADResult{Format: "stl", Rejection: CADRejectTruncated}
	}
	return CADResult{}
}
