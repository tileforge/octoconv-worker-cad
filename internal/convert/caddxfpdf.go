package convert

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

var ErrCADPDFRejected = errors.New("cad: the drawing's PDF failed the content check")

const CADDXFPDFTextFloorPt = 0.00469174

type CADPDFFacts struct {
	Pages, PaintOps, TextRunes, ExpectedRunes int
	Text                                      string
	PageW, PageH                              float64
	BelowRenderFloor, BelowFloorRunes         int
	InvisibleText, InvisibleRunes             int
	FloorPt                                   float64
	InkOps                                    int
}

type CADPDFShow struct {
	Text   string
	SizePt float64
}

type CADDXFPDFText struct {
	Text         string
	SVGPt, PDFPt float64
	Present      int
}

type cadDXFText struct {
	Text, Covered string
	Invisible     bool
}

const (
	cadPDFInflateFactor = 64
	cadPDFMaxObjects    = 1 << 16
	cadPDFMaxValues     = 1 << 22
	cadPDFMaxDepth      = 32
	cadPDFMaxOperands   = 1 << 12
	cadPDFMaxString     = 1 << 20
	cadPDFMaxWord       = 1 << 10
	cadPDFMaxText       = 16 << 20
	cadPDFMaxCMap       = 1 << 17
	cadPDFMaxRefChain   = 16
	cadPDFBufSize       = 1 << 16
)

var errCADPDFInflateCap = errors.New("inflate cap")

func cadPDFReject(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCADPDFRejected, fmt.Sprintf(format, args...))
}

func cadStripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func cadRuneSubsequence(sub, s string) bool {
	rest := s
	for _, r := range sub {
		i := strings.IndexRune(rest, r)
		if i < 0 {
			return false
		}
		_, n := utf8.DecodeRuneInString(rest[i:])
		rest = rest[i+n:]
	}
	return true
}

func cadRuneSubsequenceCount(sub, s string) int {
	rest, found := s, 0
	for _, r := range sub {
		i := strings.IndexRune(rest, r)
		if i < 0 {
			continue
		}
		_, n := utf8.DecodeRuneInString(rest[i:])
		rest = rest[i+n:]
		found++
	}
	return found
}

type cadPDFKind int

const (
	cadPDFEOF cadPDFKind = iota
	cadPDFNum
	cadPDFNameTok
	cadPDFStr
	cadPDFKw
	cadPDFDictOpen
	cadPDFDictClose
	cadPDFArrOpen
	cadPDFArrClose
	cadPDFBraceOpen
	cadPDFBraceClose
)

type cadPDFTok struct {
	kind  cadPDFKind
	text  string
	str   []byte
	num   float64
	isInt bool
	ival  int64
}

type cadPDFName string

type cadPDFString []byte

type cadPDFRef struct{ num, gen int }

type cadPDFLexer struct {
	r      *bufio.Reader
	pos    int64
	ioErr  error
	stream bool
	back   []cadPDFTok
}

func cadPDFIsSpace(c byte) bool {
	return c == 0 || c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}

func cadPDFIsDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func (l *cadPDFLexer) readByte() (byte, bool) {
	c, err := l.r.ReadByte()
	if err != nil {
		if err != io.EOF && l.ioErr == nil {
			l.ioErr = err
		}
		return 0, false
	}
	l.pos++
	return c, true
}

func (l *cadPDFLexer) unreadByte() {
	if l.r.UnreadByte() == nil {
		l.pos--
	}
}

func (l *cadPDFLexer) failed(fallback error) error {
	if l.ioErr == nil {
		return fallback
	}
	if errors.Is(l.ioErr, errCADPDFInflateCap) {
		return fmt.Errorf("%w: %w", ErrCADTooComplex, cadPDFReject("the inflated content exceeds %d times the PDF byte limit", cadPDFInflateFactor))
	}
	if l.stream {
		return cadPDFReject("corrupt stream: %v", l.ioErr)
	}
	return fmt.Errorf("cad: read the PDF: %w", l.ioErr)
}

func (l *cadPDFLexer) push(t cadPDFTok) { l.back = append(l.back, t) }

func (l *cadPDFLexer) next() (cadPDFTok, error) {
	if n := len(l.back); n > 0 {
		t := l.back[n-1]
		l.back = l.back[:n-1]
		return t, nil
	}
	for {
		c, ok := l.readByte()
		if !ok {
			return cadPDFTok{kind: cadPDFEOF}, l.failed(nil)
		}
		if cadPDFIsSpace(c) {
			continue
		}
		switch c {
		case '%':
			for {
				d, ok := l.readByte()
				if !ok {
					return cadPDFTok{kind: cadPDFEOF}, l.failed(nil)
				}
				if d == '\n' || d == '\r' {
					break
				}
			}
			continue
		case '(':
			return l.literal()
		case '<':
			d, ok := l.readByte()
			if ok && d == '<' {
				return cadPDFTok{kind: cadPDFDictOpen}, nil
			}
			if ok {
				l.unreadByte()
			}
			return l.hex()
		case '>':
			d, ok := l.readByte()
			if ok && d == '>' {
				return cadPDFTok{kind: cadPDFDictClose}, nil
			}
			return cadPDFTok{}, l.failed(cadPDFReject("a stray > at byte %d", l.pos))
		case '[':
			return cadPDFTok{kind: cadPDFArrOpen}, nil
		case ']':
			return cadPDFTok{kind: cadPDFArrClose}, nil
		case '{':
			return cadPDFTok{kind: cadPDFBraceOpen}, nil
		case '}':
			return cadPDFTok{kind: cadPDFBraceClose}, nil
		case ')':
			return cadPDFTok{}, cadPDFReject("a stray ) at byte %d", l.pos)
		case '/':
			return l.name()
		}
		return l.word(c)
	}
}

func (l *cadPDFLexer) literal() (cadPDFTok, error) {
	var b []byte
	depth := 1
	unterminated := cadPDFReject("unterminated literal string")
	for {
		if len(b) > cadPDFMaxString {
			return cadPDFTok{}, fmt.Errorf("%w: %w", ErrCADTooComplex, cadPDFReject("a string longer than %d bytes", cadPDFMaxString))
		}
		c, ok := l.readByte()
		if !ok {
			return cadPDFTok{}, l.failed(unterminated)
		}
		switch c {
		case '(':
			depth++
			b = append(b, c)
		case ')':
			depth--
			if depth == 0 {
				return cadPDFTok{kind: cadPDFStr, str: b}, nil
			}
			b = append(b, c)
		case '\r':
			if d, ok := l.readByte(); ok && d != '\n' {
				l.unreadByte()
			}
			b = append(b, '\n')
		case '\\':
			e, ok := l.readByte()
			if !ok {
				return cadPDFTok{}, l.failed(unterminated)
			}
			switch e {
			case 'n':
				b = append(b, '\n')
			case 'r':
				b = append(b, '\r')
			case 't':
				b = append(b, '\t')
			case 'b':
				b = append(b, '\b')
			case 'f':
				b = append(b, '\f')
			case '\r':
				if d, ok := l.readByte(); ok && d != '\n' {
					l.unreadByte()
				}
			case '\n':
			default:
				if e >= '0' && e <= '7' {
					v := int(e - '0')
					for i := 0; i < 2; i++ {
						d, ok := l.readByte()
						if !ok {
							break
						}
						if d < '0' || d > '7' {
							l.unreadByte()
							break
						}
						v = v*8 + int(d-'0')
					}
					b = append(b, byte(v))
				} else {
					b = append(b, e)
				}
			}
		default:
			b = append(b, c)
		}
	}
}

func (l *cadPDFLexer) hex() (cadPDFTok, error) {
	var b []byte
	var hi byte
	half := false
	for {
		if len(b) > cadPDFMaxString {
			return cadPDFTok{}, fmt.Errorf("%w: %w", ErrCADTooComplex, cadPDFReject("a string longer than %d bytes", cadPDFMaxString))
		}
		c, ok := l.readByte()
		if !ok {
			return cadPDFTok{}, l.failed(cadPDFReject("unterminated hex string"))
		}
		if c == '>' {
			if half {
				b = append(b, hi<<4)
			}
			return cadPDFTok{kind: cadPDFStr, str: b}, nil
		}
		if cadPDFIsSpace(c) {
			continue
		}
		v, ok := pdfHexDigit(c)
		if !ok {
			return cadPDFTok{}, cadPDFReject("a bad hex digit %q", c)
		}
		if half {
			b = append(b, hi<<4|v)
			half = false
		} else {
			hi, half = v, true
		}
	}
}

func (l *cadPDFLexer) run(first []byte) ([]byte, error) {
	b := first
	for {
		if len(b) > cadPDFMaxWord {
			return nil, cadPDFReject("a token longer than %d bytes", cadPDFMaxWord)
		}
		c, ok := l.readByte()
		if !ok {
			if l.ioErr != nil {
				return nil, l.failed(nil)
			}
			return b, nil
		}
		if cadPDFIsSpace(c) || cadPDFIsDelim(c) {
			l.unreadByte()
			return b, nil
		}
		b = append(b, c)
	}
}

func (l *cadPDFLexer) name() (cadPDFTok, error) {
	raw, err := l.run(nil)
	if err != nil {
		return cadPDFTok{}, err
	}
	var b []byte
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i+2 < len(raw) {
			h, ok1 := pdfHexDigit(raw[i+1])
			lo, ok2 := pdfHexDigit(raw[i+2])
			if ok1 && ok2 {
				b = append(b, h<<4|lo)
				i += 2
				continue
			}
		}
		b = append(b, raw[i])
	}
	return cadPDFTok{kind: cadPDFNameTok, text: string(b)}, nil
}

func cadPDFNumber(s string) (cadPDFTok, bool) {
	digits, dots := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			dots++
		case (c == '+' || c == '-') && i == 0:
		default:
			return cadPDFTok{}, false
		}
	}
	if digits == 0 || dots > 1 {
		return cadPDFTok{}, false
	}
	if dots == 0 {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return cadPDFTok{kind: cadPDFNum, num: float64(v), isInt: true, ival: v}, true
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return cadPDFTok{}, false
	}
	return cadPDFTok{kind: cadPDFNum, num: v}, true
}

func (l *cadPDFLexer) word(first byte) (cadPDFTok, error) {
	raw, err := l.run([]byte{first})
	if err != nil {
		return cadPDFTok{}, err
	}
	if t, ok := cadPDFNumber(string(raw)); ok {
		return t, nil
	}
	return cadPDFTok{kind: cadPDFKw, text: string(raw)}, nil
}

func cadPDFDescribe(t cadPDFTok) string {
	switch t.kind {
	case cadPDFEOF:
		return "end of data"
	case cadPDFNum:
		return strconv.FormatFloat(t.num, 'g', -1, 64)
	case cadPDFNameTok:
		return "/" + cadSVGClip(t.text)
	case cadPDFStr:
		return "a string"
	case cadPDFKw:
		return cadSVGClip(t.text)
	}
	return "a delimiter"
}

func (l *cadPDFLexer) value(t cadPDFTok, depth int, refs bool, count *int) (any, error) {
	if count != nil {
		*count++
		if *count > cadPDFMaxValues {
			return nil, fmt.Errorf("%w: %w", ErrCADTooComplex, cadPDFReject("more than %d values outside streams", cadPDFMaxValues))
		}
	}
	if depth > cadPDFMaxDepth {
		return nil, cadPDFReject("objects nested deeper than %d", cadPDFMaxDepth)
	}
	switch t.kind {
	case cadPDFNum:
		if t.isInt && refs {
			t2, err := l.next()
			if err != nil {
				return nil, err
			}
			if t2.kind == cadPDFNum && t2.isInt {
				t3, err := l.next()
				if err != nil {
					return nil, err
				}
				if t3.kind == cadPDFKw && t3.text == "R" {
					return cadPDFRef{int(t.ival), int(t2.ival)}, nil
				}
				l.push(t3)
			}
			l.push(t2)
		}
		if t.isInt {
			return t.ival, nil
		}
		return t.num, nil
	case cadPDFNameTok:
		return cadPDFName(t.text), nil
	case cadPDFStr:
		return cadPDFString(t.str), nil
	case cadPDFArrOpen:
		var arr []any
		for {
			e, err := l.next()
			if err != nil {
				return nil, err
			}
			if e.kind == cadPDFArrClose {
				return arr, nil
			}
			if e.kind == cadPDFEOF {
				return nil, cadPDFReject("unterminated array")
			}
			v, err := l.value(e, depth+1, refs, count)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
	case cadPDFDictOpen:
		dict := map[string]any{}
		for {
			k, err := l.next()
			if err != nil {
				return nil, err
			}
			if k.kind == cadPDFDictClose {
				return dict, nil
			}
			if k.kind != cadPDFNameTok {
				return nil, cadPDFReject("a dictionary key is %s, not a name", cadPDFDescribe(k))
			}
			vt, err := l.next()
			if err != nil {
				return nil, err
			}
			if vt.kind == cadPDFDictClose || vt.kind == cadPDFEOF {
				return nil, cadPDFReject("dictionary key /%s has no value", cadSVGClip(k.text))
			}
			v, err := l.value(vt, depth+1, refs, count)
			if err != nil {
				return nil, err
			}
			dict[k.text] = v
		}
	case cadPDFKw:
		switch t.text {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
	}
	return nil, cadPDFReject("unexpected token %s", cadPDFDescribe(t))
}

type cadPDFObj struct {
	val     any
	stream  bool
	dataOff int64
	length  int64
	lenRef  *cadPDFRef
	kwOff   int64
}

type cadPDFDoc struct {
	f       *os.File
	size    int64
	objs    map[int]*cadPDFObj
	trailer map[string]any
	budget  int64
	values  int
}

func (d *cadPDFDoc) lexerAt(off int64) *cadPDFLexer {
	return &cadPDFLexer{r: bufio.NewReaderSize(io.NewSectionReader(d.f, off, d.size-off), cadPDFBufSize), pos: off}
}

func (d *cadPDFDoc) findEndstream(from int64) (int64, error) {
	kw := []byte("endstream")
	chunk := make([]byte, cadPDFBufSize)
	var buf []byte
	base := from
	for pos := from; pos < d.size; {
		n, err := d.f.ReadAt(chunk[:min(int64(len(chunk)), d.size-pos)], pos)
		if n == 0 && err != nil {
			return -1, fmt.Errorf("cad: read the PDF: %w", err)
		}
		buf = append(buf, chunk[:n]...)
		pos += int64(n)
		if i := bytes.Index(buf, kw); i >= 0 {
			return base + int64(i), nil
		}
		keep := min(len(buf), len(kw)-1)
		base += int64(len(buf) - keep)
		buf = append(buf[:0], buf[len(buf)-keep:]...)
	}
	return -1, nil
}

func (d *cadPDFDoc) expectKeyword(l *cadPDFLexer, kw, what string) error {
	t, err := l.next()
	if err != nil {
		return err
	}
	if t.kind != cadPDFKw || t.text != kw {
		return cadPDFReject("%s: found %s where %s belongs", what, cadPDFDescribe(t), kw)
	}
	return nil
}

func (d *cadPDFDoc) scan() error {
	l := d.lexerAt(0)
	head := make([]byte, 0, 5)
	for len(head) < 5 {
		c, ok := l.readByte()
		if !ok {
			break
		}
		head = append(head, c)
	}
	if string(head) != "%PDF-" {
		return l.failed(cadPDFReject("the file does not start with %%PDF-"))
	}
	for {
		c, ok := l.readByte()
		if !ok || c == '\n' || c == '\r' {
			break
		}
	}
	seenXref := false
	for {
		t, err := l.next()
		if err != nil {
			return err
		}
		switch {
		case t.kind == cadPDFEOF:
			if !seenXref {
				return cadPDFReject("no trailer")
			}
			return nil
		case t.kind == cadPDFNum && t.isInt:
			num := int(t.ival)
			g, err := l.next()
			if err != nil {
				return err
			}
			if g.kind != cadPDFNum || !g.isInt {
				return cadPDFReject("unexpected token %s after %d at the top level", cadPDFDescribe(g), num)
			}
			if err := d.expectKeyword(l, "obj", fmt.Sprintf("object %d", num)); err != nil {
				return err
			}
			if num <= 0 {
				return cadPDFReject("object number %d", num)
			}
			if _, dup := d.objs[num]; dup {
				return cadPDFReject("object %d defined twice", num)
			}
			if len(d.objs) >= cadPDFMaxObjects {
				return fmt.Errorf("%w: %w", ErrCADTooComplex, cadPDFReject("more than %d objects", cadPDFMaxObjects))
			}
			vt, err := l.next()
			if err != nil {
				return err
			}
			val, err := l.value(vt, 0, true, &d.values)
			if err != nil {
				return err
			}
			o := &cadPDFObj{val: val, length: -1}
			d.objs[num] = o
			end, err := l.next()
			if err != nil {
				return err
			}
			if end.kind == cadPDFKw && end.text == "endobj" {
				continue
			}
			if end.kind != cadPDFKw || end.text != "stream" {
				return cadPDFReject("object %d is closed by %s, not endobj", num, cadPDFDescribe(end))
			}
			dict, isDict := val.(map[string]any)
			if !isDict || len(l.back) > 0 {
				return cadPDFReject("object %d has a stream without a dictionary", num)
			}
			o.stream = true
			c, ok := l.readByte()
			if ok && c == '\r' {
				c, ok = l.readByte()
			}
			if !ok || c != '\n' {
				return l.failed(cadPDFReject("object %d: the stream keyword is not followed by an end of line", num))
			}
			o.dataOff = l.pos
			switch n := dict["Length"].(type) {
			case int64:
				if n < 0 || o.dataOff+n > d.size {
					return cadPDFReject("object %d: stream length %d runs past the file", num, n)
				}
				o.length = n
				l = d.lexerAt(o.dataOff + n)
				if err := d.expectKeyword(l, "endstream", fmt.Sprintf("object %d: stream length %d", num, n)); err != nil {
					return err
				}
			case cadPDFRef:
				kw, err := d.findEndstream(o.dataOff)
				if err != nil {
					return err
				}
				if kw < 0 {
					return cadPDFReject("object %d: no endstream", num)
				}
				o.lenRef, o.kwOff = &n, kw
				l = d.lexerAt(kw + int64(len("endstream")))
			default:
				return cadPDFReject("object %d: stream /Length is missing or not an integer", num)
			}
			if err := d.expectKeyword(l, "endobj", fmt.Sprintf("object %d", num)); err != nil {
				return err
			}
		case t.kind == cadPDFKw && t.text == "xref":
			if seenXref {
				return cadPDFReject("more than one cross-reference section")
			}
			for {
				e, err := l.next()
				if err != nil {
					return err
				}
				if e.kind == cadPDFKw && e.text == "trailer" {
					break
				}
				if !(e.kind == cadPDFNum && e.isInt) && !(e.kind == cadPDFKw && (e.text == "n" || e.text == "f")) {
					return cadPDFReject("malformed cross-reference table at %s", cadPDFDescribe(e))
				}
			}
			dt, err := l.next()
			if err != nil {
				return err
			}
			tv, err := l.value(dt, 0, true, &d.values)
			if err != nil {
				return err
			}
			tr, ok := tv.(map[string]any)
			if !ok {
				return cadPDFReject("the trailer is not a dictionary")
			}
			d.trailer = tr
			seenXref = true
		case t.kind == cadPDFKw && t.text == "startxref":
			n, err := l.next()
			if err != nil {
				return err
			}
			if n.kind != cadPDFNum || !n.isInt {
				return cadPDFReject("startxref is followed by %s", cadPDFDescribe(n))
			}
		default:
			return cadPDFReject("unexpected token %s at the top level", cadPDFDescribe(t))
		}
	}
}

func (d *cadPDFDoc) resolveLengths() error {
	nums := d.sortedNums()
	for _, num := range nums {
		o := d.objs[num]
		if !o.stream || o.lenRef == nil {
			continue
		}
		var v int64 = -1
		if lo, ok := d.objs[o.lenRef.num]; ok && !lo.stream {
			if n, isInt := lo.val.(int64); isInt {
				v = n
			}
		}
		if v < 0 {
			return cadPDFReject("object %d: stream length reference %d 0 R is not an integer", num, o.lenRef.num)
		}
		end := o.dataOff + v
		gap := o.kwOff - end
		fits := false
		if gap >= 0 && gap <= 2 {
			eol := make([]byte, gap)
			if _, err := d.f.ReadAt(eol, end); err != nil {
				return fmt.Errorf("cad: read the PDF: %w", err)
			}
			fits = gap == 0 || string(eol) == "\n" || string(eol) == "\r" || string(eol) == "\r\n"
		}
		if !fits {
			return cadPDFReject("object %d: stream length %d does not end at endstream", num, v)
		}
		o.length = v
	}
	return nil
}

func (d *cadPDFDoc) sortedNums() []int {
	nums := make([]int, 0, len(d.objs))
	for n := range d.objs {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	return nums
}

func (d *cadPDFDoc) resolve(v any) (any, *cadPDFObj, error) {
	for i := 0; i < cadPDFMaxRefChain; i++ {
		r, ok := v.(cadPDFRef)
		if !ok {
			return v, nil, nil
		}
		o, ok := d.objs[r.num]
		if !ok {
			return nil, nil, cadPDFReject("reference to missing object %d", r.num)
		}
		if o.stream {
			return o.val, o, nil
		}
		v = o.val
	}
	return nil, nil, cadPDFReject("a reference chain longer than %d", cadPDFMaxRefChain)
}

func (d *cadPDFDoc) dict(v any) (map[string]any, error) {
	r, _, err := d.resolve(v)
	if err != nil || r == nil {
		return nil, err
	}
	m, ok := r.(map[string]any)
	if !ok {
		return nil, cadPDFReject("a dictionary was expected")
	}
	return m, nil
}

type cadPDFCapReader struct {
	r      io.Reader
	budget *int64
}

func (c *cadPDFCapReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.budget -= int64(n)
	if *c.budget < 0 {
		return n, errCADPDFInflateCap
	}
	return n, err
}

func (d *cadPDFDoc) open(o *cadPDFObj, what string) (*cadPDFLexer, error) {
	dict, _ := o.val.(map[string]any)
	if _, ok := dict["DecodeParms"]; ok {
		return nil, cadPDFReject("%s carries /DecodeParms", what)
	}
	if _, ok := dict["DP"]; ok {
		return nil, cadPDFReject("%s carries /DecodeParms", what)
	}
	filter, _, err := d.resolve(dict["Filter"])
	if err != nil {
		return nil, err
	}
	if arr, ok := filter.([]any); ok && len(arr) == 1 {
		filter = arr[0]
	}
	var r io.Reader = io.NewSectionReader(d.f, o.dataOff, o.length)
	switch filter {
	case nil:
	case cadPDFName("FlateDecode"):
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, cadPDFReject("%s: corrupt Flate stream: %v", what, err)
		}
		r = zr
	default:
		return nil, cadPDFReject("%s: unsupported filter %v", what, filter)
	}
	return &cadPDFLexer{r: bufio.NewReaderSize(&cadPDFCapReader{r: r, budget: &d.budget}, cadPDFBufSize), stream: true}, nil
}

type cadPDFFont struct {
	two  bool
	cmap map[uint32]string
}

func cadPDFUTF16(b []byte) (string, error) {
	if len(b) == 0 || len(b)%2 != 0 {
		return "", cadPDFReject("a ToUnicode destination of %d bytes", len(b))
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return string(utf16.Decode(u)), nil
}

func cadPDFCode(b []byte) (uint32, error) {
	if len(b) == 0 || len(b) > 4 {
		return 0, cadPDFReject("a ToUnicode source code of %d bytes", len(b))
	}
	var v uint32
	for _, c := range b {
		v = v<<8 | uint32(c)
	}
	return uint32(len(b))<<24 | v, nil
}

func (d *cadPDFDoc) parseCMap(o *cadPDFObj, name string) (map[uint32]string, error) {
	l, err := d.open(o, "the ToUnicode CMap of font /"+name)
	if err != nil {
		return nil, err
	}
	cmap := map[uint32]string{}
	add := func(code uint32, s string) error {
		if len(cmap) >= cadPDFMaxCMap {
			return cadPDFReject("a ToUnicode CMap with more than %d entries", cadPDFMaxCMap)
		}
		cmap[code] = s
		return nil
	}
	str := func(what string) ([]byte, error) {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		if t.kind != cadPDFStr {
			return nil, cadPDFReject("ToUnicode %s is %s, not a string", what, cadPDFDescribe(t))
		}
		return t.str, nil
	}
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		if t.kind == cadPDFEOF {
			return cmap, nil
		}
		if t.kind != cadPDFKw {
			continue
		}
		switch t.text {
		case "beginbfchar":
			for {
				e, err := l.next()
				if err != nil {
					return nil, err
				}
				if e.kind == cadPDFKw && e.text == "endbfchar" {
					break
				}
				if e.kind != cadPDFStr {
					return nil, cadPDFReject("ToUnicode bfchar source is %s", cadPDFDescribe(e))
				}
				code, err := cadPDFCode(e.str)
				if err != nil {
					return nil, err
				}
				dst, err := str("bfchar destination")
				if err != nil {
					return nil, err
				}
				s, err := cadPDFUTF16(dst)
				if err != nil {
					return nil, err
				}
				if err := add(code, s); err != nil {
					return nil, err
				}
			}
		case "beginbfrange":
			for {
				e, err := l.next()
				if err != nil {
					return nil, err
				}
				if e.kind == cadPDFKw && e.text == "endbfrange" {
					break
				}
				if e.kind != cadPDFStr {
					return nil, cadPDFReject("ToUnicode bfrange start is %s", cadPDFDescribe(e))
				}
				hiB, err := str("bfrange end")
				if err != nil {
					return nil, err
				}
				lo, err := cadPDFCode(e.str)
				if err != nil {
					return nil, err
				}
				hi, err := cadPDFCode(hiB)
				if err != nil {
					return nil, err
				}
				if lo>>24 != hi>>24 || hi < lo || hi-lo >= cadPDFMaxCMap {
					return nil, cadPDFReject("a ToUnicode bfrange from %x to %x", lo, hi)
				}
				n := int(hi-lo) + 1
				dt, err := l.next()
				if err != nil {
					return nil, err
				}
				switch dt.kind {
				case cadPDFStr:
					if len(dt.str) == 0 || len(dt.str)%2 != 0 {
						return nil, cadPDFReject("a ToUnicode destination of %d bytes", len(dt.str))
					}
					units := make([]uint16, len(dt.str)/2)
					for i := range units {
						units[i] = uint16(dt.str[2*i])<<8 | uint16(dt.str[2*i+1])
					}
					last := units[len(units)-1]
					for i := 0; i < n; i++ {
						units[len(units)-1] = last + uint16(i)
						if err := add(lo+uint32(i), string(utf16.Decode(units))); err != nil {
							return nil, err
						}
					}
				case cadPDFArrOpen:
					v, err := l.value(dt, 0, false, nil)
					if err != nil {
						return nil, err
					}
					arr, _ := v.([]any)
					if len(arr) != n {
						return nil, cadPDFReject("a ToUnicode bfrange array of %d for %d codes", len(arr), n)
					}
					for i, a := range arr {
						s, ok := a.(cadPDFString)
						if !ok {
							return nil, cadPDFReject("a ToUnicode bfrange array entry is not a string")
						}
						u, err := cadPDFUTF16(s)
						if err != nil {
							return nil, err
						}
						if err := add(lo+uint32(i), u); err != nil {
							return nil, err
						}
					}
				default:
					return nil, cadPDFReject("ToUnicode bfrange destination is %s", cadPDFDescribe(dt))
				}
			}
		}
	}
}

var cadPDFPaintOps = map[string]bool{
	"S": true, "s": true, "f": true, "F": true, "f*": true, "B": true, "B*": true, "b": true, "b*": true,
	"Tj": true, "TJ": true, "'": true, `"`: true,
}

var cadPDFOtherOps = map[string]bool{
	"q": true, "Q": true, "cm": true, "w": true, "J": true, "j": true, "M": true, "d": true, "ri": true, "i": true, "gs": true,
	"m": true, "l": true, "c": true, "v": true, "y": true, "h": true, "re": true, "n": true, "W": true, "W*": true,
	"BT": true, "ET": true, "Tc": true, "Tw": true, "Tz": true, "TL": true, "Tf": true, "Tr": true, "Ts": true,
	"Td": true, "TD": true, "Tm": true, "T*": true, "d0": true, "d1": true,
	"CS": true, "cs": true, "SC": true, "SCN": true, "sc": true, "scn": true, "G": true, "g": true, "RG": true, "rg": true, "K": true, "k": true,
	"BMC": true, "BDC": true, "EMC": true, "MP": true, "DP": true, "BX": true, "EX": true,
}

type cadPDFGState struct {
	ctm         [4]float64
	strokeAlpha float64
	fillAlpha   float64
	softMask    bool
	render      int64
}

type cadPDFRun struct {
	d      *cadPDFDoc
	fonts  map[string]any
	extg   map[string]any
	cache  map[string]*cadPDFFont
	cur    *cadPDFFont
	text   strings.Builder
	paint  int
	ink    int
	keep   bool
	shows  []CADPDFShow
	gs     cadPDFGState
	stack  []cadPDFGState
	tm     [4]float64
	tfSize float64
}

func (r *cadPDFRun) strokes() bool { return !r.gs.softMask && r.gs.strokeAlpha > 0 }

func (r *cadPDFRun) fills() bool { return !r.gs.softMask && r.gs.fillAlpha > 0 }

func (r *cadPDFRun) textInk() bool {
	switch r.gs.render {
	case 0, 4:
		return r.fills()
	case 1, 5:
		return r.strokes()
	case 2, 6:
		return r.fills() || r.strokes()
	}
	return false
}

func (r *cadPDFRun) setState(args []any) error {
	if len(args) != 1 {
		return cadPDFReject("gs with %d operands", len(args))
	}
	n, ok := args[0].(cadPDFName)
	if !ok {
		return cadPDFReject("gs without an ExtGState name")
	}
	entry, ok := r.extg[string(n)]
	if !ok {
		return cadPDFReject("ExtGState /%s is not in the page resources", cadSVGClip(string(n)))
	}
	v, _, err := r.d.resolve(entry)
	if err != nil {
		return err
	}
	gd, ok := v.(map[string]any)
	if !ok {
		return cadPDFReject("ExtGState /%s is not a dictionary", cadSVGClip(string(n)))
	}
	for _, a := range []struct {
		key string
		dst *float64
	}{{"CA", &r.gs.strokeAlpha}, {"ca", &r.gs.fillAlpha}} {
		raw, present := gd[a.key]
		if !present {
			continue
		}
		rv, _, err := r.d.resolve(raw)
		if err != nil {
			return err
		}
		f, ok := cadPDFNumber64(rv)
		if !ok || math.IsNaN(f) || f < 0 || f > 1 {
			return cadPDFReject("ExtGState /%s has /%s %v, not a number in [0, 1]", cadSVGClip(string(n)), a.key, rv)
		}
		*a.dst = f
	}
	if raw, present := gd["SMask"]; present {
		rv, _, err := r.d.resolve(raw)
		if err != nil {
			return err
		}
		r.gs.softMask = rv != cadPDFName("None")
	}
	return nil
}

func cadPDFNumber64(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func cadPDFMatrix(args []any) ([4]float64, bool) {
	var m [4]float64
	if len(args) != 6 {
		return m, false
	}
	for i := range m {
		f, ok := cadPDFNumber64(args[i])
		if !ok {
			return m, false
		}
		m[i] = f
	}
	return m, true
}

func cadPDFMul(a, b [4]float64) [4]float64 {
	return [4]float64{a[0]*b[0] + a[1]*b[2], a[0]*b[1] + a[1]*b[3], a[2]*b[0] + a[3]*b[2], a[2]*b[1] + a[3]*b[3]}
}

func (r *cadPDFRun) sizePt() float64 {
	m := cadPDFMul(r.tm, r.gs.ctm)
	return math.Abs(r.tfSize) * math.Sqrt(math.Abs(m[0]*m[3]-m[1]*m[2]))
}

func (r *cadPDFRun) font(name string) (*cadPDFFont, error) {
	if f, ok := r.cache[name]; ok {
		return f, nil
	}
	entry, ok := r.fonts[name]
	if !ok {
		return nil, cadPDFReject("font /%s is not in the page resources", cadSVGClip(name))
	}
	fd, err := r.d.dict(entry)
	if err != nil {
		return nil, err
	}
	if fd == nil {
		return nil, cadPDFReject("font /%s is not a dictionary", cadSVGClip(name))
	}
	f := &cadPDFFont{}
	switch fd["Subtype"] {
	case cadPDFName("Type0"):
		enc, _, err := r.d.resolve(fd["Encoding"])
		if err != nil {
			return nil, err
		}
		if enc != cadPDFName("Identity-H") {
			return nil, cadPDFReject("font /%s encoding %v is not Identity-H", cadSVGClip(name), enc)
		}
		f.two = true
	case cadPDFName("TrueType"), cadPDFName("Type1"), cadPDFName("MMType1"), cadPDFName("Type3"):
	default:
		return nil, cadPDFReject("font /%s subtype %v is not supported", cadSVGClip(name), fd["Subtype"])
	}
	_, tu, err := r.d.resolve(fd["ToUnicode"])
	if err != nil {
		return nil, err
	}
	if tu == nil {
		return nil, cadPDFReject("font /%s has no ToUnicode CMap", cadSVGClip(name))
	}
	f.cmap, err = r.d.parseCMap(tu, cadSVGClip(name))
	if err != nil {
		return nil, err
	}
	r.cache[name] = f
	return f, nil
}

func (r *cadPDFRun) show(v any) (int, error) {
	s, ok := v.(cadPDFString)
	if !ok {
		return 0, cadPDFReject("a text-show operand is not a string")
	}
	if r.cur == nil {
		return 0, cadPDFReject("text shown without a font")
	}
	width := 1
	if r.cur.two {
		width = 2
		if len(s)%2 != 0 {
			return 0, cadPDFReject("an odd-length string for a two-byte font")
		}
	}
	start := r.text.Len()
	for i := 0; i+width <= len(s); i += width {
		code := uint32(width) << 24
		if width == 2 {
			code |= uint32(s[i])<<8 | uint32(s[i+1])
		} else {
			code |= uint32(s[i])
		}
		r.text.WriteString(r.cur.cmap[code])
	}
	if r.keep {
		r.shows = append(r.shows, CADPDFShow{Text: r.text.String()[start:], SizePt: r.sizePt()})
	}
	if r.text.Len() > cadPDFMaxText {
		return 0, fmt.Errorf("%w: %w", ErrCADTooComplex, cadPDFReject("more than %d bytes of decoded text", cadPDFMaxText))
	}
	return len(s) / width, nil
}

func (r *cadPDFRun) shown(n int) {
	if n > 0 && r.textInk() {
		r.ink++
	}
}

func (r *cadPDFRun) op(name string, args []any) error {
	if !cadPDFPaintOps[name] && !cadPDFOtherOps[name] {
		return cadPDFReject("unsupported content operator %s", cadSVGClip(name))
	}
	if cadPDFPaintOps[name] {
		r.paint++
	}
	last := func() any {
		if len(args) == 0 {
			return nil
		}
		return args[len(args)-1]
	}
	switch name {
	case "S", "s":
		if r.strokes() {
			r.ink++
		}
	case "f", "F", "f*":
		if r.fills() {
			r.ink++
		}
	case "B", "B*", "b", "b*":
		if r.fills() || r.strokes() {
			r.ink++
		}
	case "gs":
		return r.setState(args)
	case "Tr":
		mode, ok := last().(int64)
		if len(args) != 1 || !ok || mode < 0 || mode > 7 {
			return cadPDFReject("Tr %v is not a text rendering mode", last())
		}
		r.gs.render = mode
	case "Tf":
		if len(args) != 2 {
			return cadPDFReject("Tf with %d operands", len(args))
		}
		n, ok := args[0].(cadPDFName)
		if !ok {
			return cadPDFReject("Tf without a font name")
		}
		f, err := r.font(string(n))
		if err != nil {
			return err
		}
		r.cur = f
		r.tfSize, _ = cadPDFNumber64(args[1])
	case "q":
		r.stack = append(r.stack, r.gs)
	case "Q":
		if len(r.stack) > 0 {
			r.gs = r.stack[len(r.stack)-1]
			r.stack = r.stack[:len(r.stack)-1]
		}
	case "cm":
		if m, ok := cadPDFMatrix(args); ok {
			r.gs.ctm = cadPDFMul(m, r.gs.ctm)
		}
	case "BT":
		r.tm = [4]float64{1, 0, 0, 1}
	case "Tm":
		if m, ok := cadPDFMatrix(args); ok {
			r.tm = m
		}
	case "Tj", "'":
		n, err := r.show(last())
		r.shown(n)
		return err
	case `"`:
		if len(args) != 3 {
			return cadPDFReject(`" with %d operands`, len(args))
		}
		n, err := r.show(args[2])
		r.shown(n)
		return err
	case "TJ":
		arr, ok := last().([]any)
		if !ok {
			return cadPDFReject("TJ without an array")
		}
		total := 0
		for _, e := range arr {
			switch e.(type) {
			case int64, float64:
			default:
				n, err := r.show(e)
				if err != nil {
					return err
				}
				total += n
			}
		}
		r.shown(total)
	case "ET":
		r.text.WriteByte('\n')
	}
	return nil
}

func (r *cadPDFRun) content(o *cadPDFObj) error {
	l, err := r.d.open(o, "the page content")
	if err != nil {
		return err
	}
	var args []any
	for {
		t, err := l.next()
		if err != nil {
			return err
		}
		if t.kind == cadPDFEOF {
			return nil
		}
		if t.kind == cadPDFKw && t.text != "true" && t.text != "false" && t.text != "null" {
			if err := r.op(t.text, args); err != nil {
				return err
			}
			args = args[:0]
			continue
		}
		v, err := l.value(t, 0, false, nil)
		if err != nil {
			return err
		}
		if len(args) >= cadPDFMaxOperands {
			return cadPDFReject("more than %d operands before an operator", cadPDFMaxOperands)
		}
		args = append(args, v)
	}
}

func (d *cadPDFDoc) page() (map[string]any, error) {
	if _, ok := d.trailer["Encrypt"]; ok {
		return nil, cadPDFReject("the trailer names /Encrypt")
	}
	if _, ok := d.trailer["XRefStm"]; ok {
		return nil, cadPDFReject("the trailer names /XRefStm")
	}
	var pages []map[string]any
	for _, num := range d.sortedNums() {
		m, ok := d.objs[num].val.(map[string]any)
		if !ok {
			continue
		}
		switch m["Type"] {
		case cadPDFName("ObjStm"):
			return nil, cadPDFReject("object %d is an object stream (/Type /ObjStm)", num)
		case cadPDFName("XRef"):
			return nil, cadPDFReject("object %d is a cross-reference stream (/Type /XRef)", num)
		case cadPDFName("Page"):
			pages = append(pages, m)
		case cadPDFName("Pages"):
			if c, _, err := d.resolve(m["Count"]); err != nil || c != int64(1) {
				return nil, cadPDFReject("object %d is a /Pages node with /Count %v", num, m["Count"])
			}
		}
	}
	if len(pages) != 1 {
		return nil, cadPDFReject("%d page objects, want 1", len(pages))
	}
	return pages[0], nil
}

func (d *cadPDFDoc) resources(page map[string]any) (map[string]any, error) {
	node := page
	for i := 0; node != nil && i < cadPDFMaxRefChain; i++ {
		if res, ok := node["Resources"]; ok {
			return d.dict(res)
		}
		parent, err := d.dict(node["Parent"])
		if err != nil {
			return nil, err
		}
		node = parent
	}
	return nil, nil
}

func (d *cadPDFDoc) contents(page map[string]any) ([]*cadPDFObj, error) {
	v, ok := page["Contents"]
	if !ok {
		return nil, nil
	}
	items := []any{v}
	if arr, isArr := v.([]any); isArr {
		items = arr
	} else if r, _, err := d.resolve(v); err == nil {
		if arr, isArr := r.([]any); isArr {
			items = arr
		}
	}
	var out []*cadPDFObj
	for _, it := range items {
		_, o, err := d.resolve(it)
		if err != nil {
			return nil, err
		}
		if o == nil {
			return nil, cadPDFReject("the page /Contents is not a stream")
		}
		out = append(out, o)
	}
	return out, nil
}

func cadPDFDecode(path string, maxBytes int64, keep bool) (CADPDFFacts, []CADPDFShow, error) {
	var facts CADPDFFacts
	if maxBytes <= 0 {
		return facts, nil, fmt.Errorf("cad: PDF byte limit %d is not positive", maxBytes)
	}
	f, st, err := OpenJobFile(path)
	if err != nil {
		return facts, nil, fmt.Errorf("cad: open the PDF: %w", err)
	}
	defer f.Close()
	if st.Size() > maxBytes {
		return facts, nil, fmt.Errorf("%w: %w", ErrCADTooComplex, cadPDFReject("the PDF is %d bytes, over the %d byte limit", st.Size(), maxBytes))
	}
	d := &cadPDFDoc{f: f, size: st.Size(), objs: map[int]*cadPDFObj{}, budget: cadPDFInflateFactor * maxBytes}
	if err := d.scan(); err != nil {
		return facts, nil, err
	}
	if err := d.resolveLengths(); err != nil {
		return facts, nil, err
	}
	page, err := d.page()
	if err != nil {
		return facts, nil, err
	}
	facts.Pages = 1
	facts.PageW, facts.PageH = d.mediaBox(page)
	res, err := d.resources(page)
	if err != nil {
		return facts, nil, err
	}
	run := &cadPDFRun{d: d, cache: map[string]*cadPDFFont{}, keep: keep, tm: [4]float64{1, 0, 0, 1},
		gs: cadPDFGState{ctm: [4]float64{1, 0, 0, 1}, strokeAlpha: 1, fillAlpha: 1}}
	if res != nil {
		if run.fonts, err = d.dict(res["Font"]); err != nil {
			return facts, nil, err
		}
		if run.extg, err = d.dict(res["ExtGState"]); err != nil {
			return facts, nil, err
		}
	}
	streams, err := d.contents(page)
	if err != nil {
		return facts, nil, err
	}
	for _, o := range streams {
		if err := run.content(o); err != nil {
			facts.PaintOps, facts.InkOps = run.paint, run.ink
			return facts, nil, err
		}
	}
	facts.PaintOps, facts.InkOps = run.paint, run.ink
	facts.Text = run.text.String()
	facts.TextRunes = utf8.RuneCountInString(cadStripSpace(facts.Text))
	return facts, run.shows, nil
}

func (d *cadPDFDoc) mediaBox(page map[string]any) (float64, float64) {
	node := page
	for i := 0; node != nil && i < cadPDFMaxRefChain; i++ {
		if v, ok := node["MediaBox"]; ok {
			box, _, err := d.resolve(v)
			arr, isArr := box.([]any)
			if err != nil || !isArr || len(arr) != 4 {
				return 0, 0
			}
			var n [4]float64
			for j := range n {
				x, _, err := d.resolve(arr[j])
				f, ok := cadPDFNumber64(x)
				if err != nil || !ok || math.IsNaN(f) || math.IsInf(f, 0) {
					return 0, 0
				}
				n[j] = f
			}
			return math.Abs(n[2] - n[0]), math.Abs(n[3] - n[1])
		}
		parent, err := d.dict(node["Parent"])
		if err != nil {
			return 0, 0
		}
		node = parent
	}
	return 0, 0
}

func JudgeCADPDF(path string, maxBytes int64, want string) (CADPDFFacts, error) {
	want = cadStripSpace(want)
	facts, _, err := cadPDFDecode(path, maxBytes, false)
	facts.ExpectedRunes = utf8.RuneCountInString(want)
	if err != nil {
		return facts, err
	}
	stripped := cadStripSpace(facts.Text)
	if facts.InkOps-1 < 1 {
		return facts, cadPDFReject("empty page: %d paint operations of which %d show ink, want at least one beyond the background", facts.PaintOps, facts.InkOps)
	}
	if want != "" && !cadRuneSubsequence(want, stripped) {
		return facts, cadPDFReject("missing text: %d of %d expected characters found in order", cadRuneSubsequenceCount(want, stripped), facts.ExpectedRunes)
	}
	return facts, nil
}

func cadDXFPDFAlign(expect []string, shows []CADPDFShow) ([]CADDXFPDFText, int) {
	var runes []rune
	var sizes []float64
	for _, sh := range shows {
		for _, r := range cadStripSpace(sh.Text) {
			runes = append(runes, r)
			sizes = append(sizes, sh.SizePt)
		}
	}
	rows := make([]CADDXFPDFText, len(expect))
	pos := 0
	for i, e := range expect {
		t := []rune(cadStripSpace(e))
		rows[i].Text = e
		if len(t) == 0 {
			rows[i].Present = -1
			continue
		}
		if pos+len(t) <= len(runes) && string(runes[pos:pos+len(t)]) == string(t) {
			rows[i].Present = 1
			rows[i].PDFPt = sizes[pos]
			pos += len(t)
		}
	}
	return rows, len(runes) - pos
}

func cadDXFPageScale(pdf CADPDFFacts, svg CADSVGFacts) (float64, error) {
	if !(pdf.PageW > 0) || !(pdf.PageH > 0) {
		return 0, cadPDFReject("the page has no usable /MediaBox")
	}
	if !(svg.ViewBoxW > 0) || !(svg.ViewBoxH > 0) {
		return 0, cadPDFReject("the SVG has no usable viewBox")
	}
	return math.Min(pdf.PageW/svg.ViewBoxW, pdf.PageH/svg.ViewBoxH), nil
}

func cadDXFReadSVGFacts(path string, maxBytes int64) (CADSVGFacts, error) {
	f, _, err := OpenJobFile(path)
	if err != nil {
		return CADSVGFacts{}, fmt.Errorf("cad: open the SVG: %w", err)
	}
	defer f.Close()
	return ValidateCADSVG(f, io.Discard, maxBytes)
}

func JudgeCADDXFPDF(path string, maxBytes int64, texts []cadDXFText, svg CADSVGFacts) (CADPDFFacts, error) {
	facts, _, err := cadPDFDecode(path, maxBytes, false)
	facts.FloorPt = CADDXFPDFTextFloorPt
	if err != nil {
		return facts, err
	}
	if len(texts) != len(svg.TextSizes) {
		return facts, cadPDFReject("texts.json has %d entries, the SVG has %d texts", len(texts), len(svg.TextSizes))
	}
	scale, err := cadDXFPageScale(facts, svg)
	if err != nil {
		return facts, err
	}
	if len(svg.TextOpacity) != len(svg.TextSizes) {
		return facts, cadPDFReject("the SVG facts carry %d text opacities for %d texts", len(svg.TextOpacity), len(svg.TextSizes))
	}
	var want strings.Builder
	for i, t := range texts {
		invisible := svg.TextOpacity[i] == 0
		if t.Invisible != invisible {
			return facts, cadPDFReject("texts.json entry %d has invisible=%t, the SVG text's fill opacity is %v", i, t.Invisible, svg.TextOpacity[i])
		}
		c := cadStripSpace(t.Covered)
		if c == "" {
			continue
		}
		if invisible {
			facts.InvisibleText++
			facts.InvisibleRunes += utf8.RuneCountInString(c)
			continue
		}
		if svg.TextSizes[i]*scale < CADDXFPDFTextFloorPt {
			facts.BelowRenderFloor++
			facts.BelowFloorRunes += utf8.RuneCountInString(c)
			continue
		}
		want.WriteString(c)
	}
	w := want.String()
	facts.ExpectedRunes = utf8.RuneCountInString(w)
	if facts.InkOps-1 < 1 {
		return facts, cadPDFReject("empty page: %d paint operations of which %d show ink, want at least one beyond the background", facts.PaintOps, facts.InkOps)
	}
	stripped := cadStripSpace(facts.Text)
	if w != "" && !cadRuneSubsequence(w, stripped) {
		return facts, cadPDFReject("missing text: %d of %d expected characters found in order", cadRuneSubsequenceCount(w, stripped), facts.ExpectedRunes)
	}
	return facts, nil
}

func JudgeCADDXFPDFFiles(pdfPath, textsPath, svgPath string, maxBytes int64) (CADPDFFacts, error) {
	svg, err := cadDXFReadSVGFacts(svgPath, maxBytes)
	if err != nil {
		return CADPDFFacts{}, err
	}
	texts, err := parseCADDXFTexts(textsPath, maxBytes)
	if err != nil {
		return CADPDFFacts{}, err
	}
	return JudgeCADDXFPDF(pdfPath, maxBytes, texts, svg)
}

func CADDXFPDFReport(pdfPath, textsPath, svgPath string, maxBytes int64) ([]CADDXFPDFText, int, error) {
	svg, err := cadDXFReadSVGFacts(svgPath, maxBytes)
	if err != nil {
		return nil, 0, err
	}
	texts, err := parseCADDXFTexts(textsPath, maxBytes)
	if err != nil {
		return nil, 0, err
	}
	if len(texts) != len(svg.Texts) {
		return nil, 0, cadPDFReject("texts.json has %d entries, the SVG has %d texts", len(texts), len(svg.Texts))
	}
	pdf, shows, err := cadPDFDecode(pdfPath, maxBytes, true)
	if err != nil {
		return nil, 0, err
	}
	scale, err := cadDXFPageScale(pdf, svg)
	if err != nil {
		return nil, 0, err
	}
	rows, residual := cadDXFPDFAlign(svg.Texts, shows)
	for i := range rows {
		rows[i].SVGPt = svg.TextSizes[i] * scale
	}
	return rows, residual, nil
}

func parseCADDXFTexts(path string, maxBytes int64) ([]cadDXFText, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("cad: texts.json byte limit %d is not positive", maxBytes)
	}
	f, st, err := OpenJobFile(path)
	if err != nil {
		return nil, fmt.Errorf("cad: open texts.json: %w", err)
	}
	defer f.Close()
	if st.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %w", ErrCADTooComplex, cadPDFReject("texts.json exceeds %d bytes", maxBytes))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cad: read texts.json: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %w", ErrCADTooComplex, cadPDFReject("texts.json exceeds %d bytes", maxBytes))
	}
	var doc struct {
		Texts *[]struct {
			Text      *string         `json:"text"`
			Covered   *string         `json:"covered"`
			Invisible json.RawMessage `json:"invisible"`
		} `json:"texts"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, cadPDFReject("texts.json: %s", cadSVGQuote(err.Error()))
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, cadPDFReject("texts.json has trailing data")
	}
	if doc.Texts == nil {
		return nil, cadPDFReject("texts.json has no texts array")
	}
	out := make([]cadDXFText, 0, len(*doc.Texts))
	for i, e := range *doc.Texts {
		if e.Text == nil {
			return nil, cadPDFReject("texts.json entry %d has no text", i)
		}
		if e.Covered == nil {
			return nil, cadPDFReject("texts.json entry %d has no covered", i)
		}
		if !cadRuneSubsequence(*e.Covered, *e.Text) {
			return nil, cadPDFReject("texts.json entry %d: covered is not an ordered subsequence of text", i)
		}
		var invisible bool
		if e.Invisible != nil {
			if string(e.Invisible) == "null" || json.Unmarshal(e.Invisible, &invisible) != nil {
				return nil, cadPDFReject("texts.json entry %d: invisible is not a boolean", i)
			}
		}
		out = append(out, cadDXFText{Text: *e.Text, Covered: *e.Covered, Invisible: invisible})
	}
	return out, nil
}

func cadDXFExpectedText(t []cadDXFText) string {
	var b strings.Builder
	for _, e := range t {
		if e.Invisible {
			continue
		}
		b.WriteString(cadStripSpace(e.Covered))
	}
	return b.String()
}

func CADDXFExpectedTextFile(path string, maxBytes int64) (string, error) {
	t, err := parseCADDXFTexts(path, maxBytes)
	if err != nil {
		return "", err
	}
	return cadDXFExpectedText(t), nil
}
