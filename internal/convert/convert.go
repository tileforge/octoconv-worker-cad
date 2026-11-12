// Package convert defines the Converter abstraction, a registry of supported
// format pairs, and the concrete engine implementations (libvips for images).
package convert

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Engine-class identifiers (D-01/DEBT-02). This is the SINGLE compile-time
// source of truth for engine-class string values -- referenced by
// Converter.Engine implementations (LibvipsConverter, LibreOfficeConverter,
// ChromiumConverter, and the future AudioConverter), the API routing switch
// (internal/api/handlers.go), the reconciler recovery-routing switch
// (internal/reconciler/reconciler.go), and the queue-name constants
// (internal/queue/queue.go). No other file may hold a raw
// "image"/"document"/"html"/"audio" engine-class literal.
const (
	EngineImage    = "image"
	EngineDocument = "document"
	EngineHTML     = "html"
	EngineAudio    = "audio"
	EngineAV       = "av"
	EngineOCR      = "ocr"
	EnginePandoc   = "pandoc"
	// EngineProbe is the eighth class and the first that serves NO convert
	// pair at all -- it exists for the sanitize operation. The name was
	// reserved in 0001_init.sql and locked by the roadmap on 2026-08-07;
	// reusing "extract" was rejected because sanitize extracts nothing and
	// extract is needed for its own meaning in the next milestone.
	EngineProbe = "probe"
	// EngineTectonic is the ninth class: real typesetting, latex -> pdf. It is
	// the only engine whose input is a PROGRAM rather than a document -- see
	// tectonic.go for what that costs and how it is bounded.
	//
	// Added to AllEngines() only once the queue, the worker and the dispatch
	// existed. Adding it earlier broke nine contracts at once -- every member
	// of this list must have a queue, a reconciler sweep route, an upload
	// ceiling, a published timeout and an estimate source -- and, worse,
	// advertised latex -> pdf on GET /v1/formats with nothing to consume it.
	// That is the defect phase 60 shipped with sanitize; here the contracts
	// caught it in minutes.
	EngineTectonic = "tectonic"
	EngineCAD      = "cad"
)

// OperationConvert is the default job operation -- "convert a file from one
// format to another." It is the SINGLE compile-time source of truth for this
// operation's string value, the same principle already applied to
// EngineImage/EngineDocument/... above (Phase 57, D-01/D-04).
//
// Operation strings, like engine-class strings, are compared EXACTLY and
// case-sensitively, with no NormalizeFormat and no trimming applied anywhere
// in this registry: NormalizeFormat exists for file formats, not for
// operations, and "helpfully" normalizing here would silently accept
// "Convert" and carry it down into jobs.operation, a column whose CHECK
// constraint would then reject the very row the API just accepted -- the
// exact class of operator-typo defect IsKnownEngine's own doc comment
// already rejects for engine classes. Do not add normalization here.
const OperationConvert = "convert"

// OperationSanitize is the second job operation: "rebuild this file with the
// enumerated classes of construct removed." Same exact-comparison discipline
// as OperationConvert -- read that constant's doc comment before adding any
// normalization here.
//
// The target format is the source format's SANITIZED FORM, which is usually
// the source format itself -- a client asking for a disarmed copy of a PDF
// expects a PDF back -- but is not for macro-enabled OOXML: a sanitized .docm
// is a .docx, because the sanitizer rewrites its content type to the
// macro-free one (D-06). The pairs SanitizeConverter registers are what make
// that a registry fact rather than a convention the handler has to remember.
const OperationSanitize = "sanitize"

const OperationRender = "render"

// AllEngines returns the ten engine classes in a fixed, documented order --
// EngineImage, EngineDocument, EngineHTML, EngineAudio, EngineAV, EngineOCR,
// EnginePandoc, EngineProbe, EngineTectonic, EngineCAD. The order is stable because callers (cmd/manage-clients) use
// it to render operator-facing error text; a fresh slice is constructed on
// every call so a caller can never mutate the source of truth through the
// returned slice. Adding an eleventh engine class means adding it here as well
// as to the const block above -- TestAllEnginesCoversEveryEngineClass fails
// if the two ever diverge from the registry.
func AllEngines() []string {
	return []string{
		EngineImage,
		EngineDocument,
		EngineHTML,
		EngineAudio,
		EngineAV,
		EngineOCR,
		EnginePandoc,
		EngineProbe,
		EngineTectonic,
		EngineCAD,
	}
}

// IsKnownEngine reports whether engine is exactly one of the values returned
// by AllEngines. The comparison is exact and case-sensitive with no
// trimming and no normalization: engine-class strings are stored verbatim in
// jobs.engine and client_limits.engine, and NormalizeFormat exists for file
// formats, not engine classes -- applying it here would silently accept
// "Image" and write a client_limits row that loadQuotaState's equality join
// (`l.engine = $2`) can never match, which is the exact operator-typo defect
// this function exists to reject. Do not "helpfully" add normalization here.
func IsKnownEngine(engine string) bool {
	for _, e := range AllEngines() {
		if engine == e {
			return true
		}
	}
	return false
}

// Pair is an ordered (source, target) format pair, e.g. {"png", "webp"}.
// Formats are always normalized (lowercase, canonical aliases).
type Pair struct {
	From string
	To   string
}

// Converter turns a file in one format into another by shelling out to an
// external engine. inPath and outPath are local filesystem paths; the output
// format is implied by outPath's extension.
type Converter interface {
	// Pairs reports the format pairs this converter can handle.
	Pairs() []Pair
	// Convert reads inPath and writes the converted result to outPath.
	Convert(ctx context.Context, inPath, outPath string, opts map[string]any) error
	// Engine reports the engine class this converter belongs to (e.g.
	// "image", "document") -- the single source of truth for engine-class
	// routing (D-01).
	Engine() string
}

// OperationalConverter is an OPTIONAL capability a Converter may
// additionally implement (Phase 57, D-06) -- added ALONGSIDE Converter,
// never as a change to it. It follows the exact precedent of
// ProgressConverter (progress.go): a second, independent optional interface,
// type-asserted by Register/Replace, with a documented fallback when absent.
//
// None of the seven converters registered by converters.go implement this
// interface, and none of them are changed by its introduction -- they have
// no need to know it exists. A converter that implements it declares which
// operation (e.g. a future sanitize engine) its Pairs() belong to; Register
// falls back to OperationConvert for every converter that does not.
type OperationalConverter interface {
	// Operation reports the job operation this converter's pairs serve
	// (e.g. "convert", or a future "sanitize"). Compared exactly, like
	// OperationConvert -- see that constant's doc comment.
	Operation() string
}

// operationOf returns c's declared operation via the OperationalConverter
// type-assert, falling back to OperationConvert when c does not implement
// it. The assertion mirrors the shape internal/worker already uses for
// ProgressConverter (`if pc, ok := conv.(ProgressConverter); ok`).
func operationOf(c Converter) string {
	if oc, ok := c.(OperationalConverter); ok {
		return oc.Operation()
	}
	return OperationConvert
}

// EngineTakesMultipleInputs reports whether an engine can use more than one
// uploaded file for a single job.
//
// One engine can. A LaTeX project is a .tex plus its .bib and its figures, and
// the engine resolves them BY NAME from the directory the document sits in --
// measured, not assumed: under --untrusted, \input{preamble} and
// \addbibresource{refs.bib} both resolve from sibling files.
//
// Every other engine takes one file and produces one file. Listing them by
// exception rather than by allowlist would mean a new engine silently inherits
// multi-input behaviour it has no code for, so this names the one that has it.
func EngineTakesMultipleInputs(engine string) bool {
	return engine == EngineTectonic
}

var formatAliases = map[string]string{
	"jpeg": "jpg",
	"tif":  "tiff",
	"htm":  "html",
	// `.tex` is THE conventional LaTeX extension; `.latex` is rare. Without
	// this alias a normal paper.tex normalized to "tex", which matches no
	// registered pair, so every real LaTeX upload 422'd as unsupported and
	// handlers.go's `source == "latex"` content-detection branch was
	// unreachable. Same alias discipline as jpeg->jpg / tif->tiff / htm->html.
	"tex": "latex",
	"stp": "step",
	"igs": "iges",
}

// NormalizeFormat lowercases a format/extension and folds common aliases so the
// registry has a single canonical key per format.
func NormalizeFormat(f string) string {
	f = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(f), "."))
	if canonical, ok := formatAliases[f]; ok {
		return canonical
	}
	return f
}

// opKey is the registry's internal map key: a normalized (operation, from,
// to) triple (Phase 57, D-07). It is unexported -- callers never construct
// or see one; they interact with the registry exclusively through
// Lookup/LookupOp and friends. Operation is stored and compared verbatim
// (see OperationConvert's doc comment); From/To are stored already
// NormalizeFormat'd.
type opKey struct {
	Operation, From, To string
}

// Registry maps normalized (operation, from, to) triples to the converter
// that handles them (Phase 57, D-07 -- previously keyed by (from, to) pair
// alone).
type Registry struct {
	m map[opKey]Converter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[opKey]Converter)}
}

// Register adds a converter for every pair it advertises, keyed by the
// triple (operationOf(c), from, to). It PANICS if any triple it would
// register is already occupied by a different converter (Phase 57, D-08).
//
// This replaces the previous behavior ("later registrations override
// earlier ones for the same pair", a bare map assignment with no collision
// check). Register is called exclusively from init() (converters.go); a
// triple collision at that point is a BUILD defect, not runtime state --
// there is no request in flight, no client waiting, nothing to gracefully
// degrade. A service that started anyway with a silently stolen triple
// would route some future client's job to the wrong converter -- and to a
// converter that may not carry that job's expected safety limits -- with no
// observable symptom until that job runs. Panicking here converts a latent,
// silent misrouting defect into a loud, unmissable failure to start.
//
// internal/convert is a library package, and CLAUDE.md's logging convention
// forbids library code from logging (only cmd/*/main.go may log) -- so this
// startup refusal is necessarily expressed as a panic, not a log.Fatalf.
//
// If the overwrite is deliberate -- e.g. a worker cmd re-registering a
// zero-value placeholder with a configured instance -- call Replace instead.
func (r *Registry) Register(c Converter) {
	op := operationOf(c)
	for _, p := range c.Pairs() {
		key := opKey{Operation: op, From: NormalizeFormat(p.From), To: NormalizeFormat(p.To)}
		if existing, ok := r.m[key]; ok {
			panic(fmt.Sprintf(
				"convert: Register: triple (operation=%q, from=%q, to=%q) already registered to %T; refusing to silently overwrite with %T -- use Replace if this override is deliberate",
				key.Operation, key.From, key.To, existing, c,
			))
		}
		r.m[key] = c
	}
}

// Replace adds a converter for every pair it advertises, keyed by the triple
// (operationOf(c), from, to), WITHOUT the occupancy check Register performs
// (Phase 57, D-09) -- an intentional, unconditional overwrite.
//
// The sole legitimate caller today: cmd/av-worker, cmd/ocr-worker, and
// cmd/pandoc-worker each overwrite a zero-value placeholder that
// converters.go's init() already registered with a CONFIGURED instance
// carrying real safety limits (MaxSourceDuration, MaxImagePixels/MaxPages,
// MaxInputBytes read from env). That overwrite is exactly why Register
// cannot simply be "last write wins": a worker cmd that used Register here
// would panic on startup against its own converters.go placeholder, even
// though the overwrite is deliberate and known-safe. Replace makes that "I
// know I am overwriting this" explicit in the caller's code, instead of
// being carried only in a comment.
func (r *Registry) Replace(c Converter) {
	op := operationOf(c)
	for _, p := range c.Pairs() {
		key := opKey{Operation: op, From: NormalizeFormat(p.From), To: NormalizeFormat(p.To)}
		r.m[key] = c
	}
}

// LookupOp finds the converter for an (operation, from, to) triple,
// normalizing from/to but NOT op (see OperationConvert's doc comment: an
// operation's string value is never normalized, only file formats are).
func (r *Registry) LookupOp(op, from, to string) (Converter, bool) {
	c, ok := r.m[opKey{Operation: op, From: NormalizeFormat(from), To: NormalizeFormat(to)}]
	return c, ok
}

// SupportsOp reports whether an (operation, from, to) triple is convertible.
func (r *Registry) SupportsOp(op, from, to string) bool {
	_, ok := r.LookupOp(op, from, to)
	return ok
}

// EngineForOp reports the engine class that handles an (operation, from,
// to) triple, or ("", false) if the triple is unsupported.
func (r *Registry) EngineForOp(op, from, to string) (string, bool) {
	c, ok := r.LookupOp(op, from, to)
	if !ok {
		return "", false
	}
	return c.Engine(), true
}

// Lookup finds the converter for a (from, to) pair under OperationConvert,
// normalizing inputs. This two-argument form is kept, unchanged, alongside
// LookupOp (Phase 57, D-07): roughly thirty existing call sites across
// internal/convert's own tests, internal/api, and internal/worker describe
// the convert operation and have no reason to know a second dimension
// exists. It is a one-line delegation to LookupOp with OperationConvert.
func (r *Registry) Lookup(from, to string) (Converter, bool) {
	return r.LookupOp(OperationConvert, from, to)
}

// Supports reports whether a (from, to) pair is convertible under
// OperationConvert. Kept two-argument for the same reason as Lookup (D-07).
func (r *Registry) Supports(from, to string) bool {
	return r.SupportsOp(OperationConvert, from, to)
}

// EngineFor reports the engine class that handles a (from, to) pair under
// OperationConvert (D-02), or ("", false) if the pair is unsupported. Kept
// two-argument for the same reason as Lookup (D-07).
func (r *Registry) EngineFor(from, to string) (string, bool) {
	return r.EngineForOp(OperationConvert, from, to)
}

// Operations returns the sorted, unique list of operations that have at
// least one registered triple, e.g. []string{"convert"} today. A fresh
// slice is built on every call (mirrors AllEngines' own discretion) so a
// caller can never mutate the registry's derived state through the
// returned slice.
//
// This is the registry-derived source of truth for "which operations are
// accepted" (Phase 57, D-02): an operation is valid if and only if at least
// one triple is registered under it -- NOT a hardcoded Go slice, and NOT the
// jobs.operation DB CHECK constraint (which reserves-but-leaves-empty
// values like "extract"/"archive"/"inspect"/"render", and, before Phase 60
// lands, "sanitize" too). Operations reports none of those reserved-but-
// unregistered values; IsKnownOperation rejects them exactly like it would
// reject any other garbage string. Same principle GET /v1/formats already
// applies to engine capabilities: what's derivable from the registry, never
// hardcoded.
func (r *Registry) Operations() []string {
	seen := make(map[string]bool)
	for key := range r.m {
		seen[key.Operation] = true
	}
	out := make([]string, 0, len(seen))
	for op := range seen {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// IsKnownOperation reports whether op is exactly one of the values returned
// by Operations. The comparison is exact and case-sensitive, mirroring
// IsKnownEngine's own discipline: no normalization, no trimming.
func (r *Registry) IsKnownOperation(op string) bool {
	for _, o := range r.Operations() {
		if op == o {
			return true
		}
	}
	return false
}

// Classes returns every registered (from, to) pair under OperationConvert,
// grouped by engine class (D-06) -- the single registry-derived source
// GET /v1/formats walks. Each class's pairs are sorted deterministically
// (by From then To) so repeated calls and JSON encoding order stay stable.
//
// This walk of r.m filters out any triple whose Operation is not
// OperationConvert (Phase 57, D-10) -- together with OperationClasses
// below, these are now the only two walks of r.m in the package. That
// filter is exactly what keeps the "engines" key in GET /v1/formats
// byte-identical to its pre-Phase-57 shape (D-10/SC1, proven by the golden
// pinning suite in internal/api). internal/api/jobs_list_handler.go derives
// its engine-filter allowlist from this same Classes() map, so an engine
// class that serves ONLY a non-convert operation (e.g. a future "probe"
// engine in Phase 60) will not appear in that filter either -- a known,
// intentional consequence Phase 60 will revisit, not an oversight here.
func (r *Registry) Classes() map[string][]Pair {
	out := make(map[string][]Pair)
	for key, c := range r.m {
		if key.Operation != OperationConvert {
			continue
		}
		class := c.Engine()
		out[class] = append(out[class], Pair{From: key.From, To: key.To})
	}
	for class := range out {
		pairs := out[class]
		sort.Slice(pairs, func(i, j int) bool {
			if pairs[i].From != pairs[j].From {
				return pairs[i].From < pairs[j].From
			}
			return pairs[i].To < pairs[j].To
		})
		out[class] = pairs
	}
	return out
}

// OperationClasses returns the full registry-derived tree
// {operation -> engine -> []Pair}, with the same deterministic per-engine
// pair sort (by From then To) that Classes applies. This is the source for
// the new operations-keyed tree GET /v1/formats gains in Phase 57 Plan 04 --
// unlike Classes, it is NOT filtered to OperationConvert; every registered
// operation, including "convert" itself, appears here.
func (r *Registry) OperationClasses() map[string]map[string][]Pair {
	out := make(map[string]map[string][]Pair)
	for key, c := range r.m {
		class := c.Engine()
		if out[key.Operation] == nil {
			out[key.Operation] = make(map[string][]Pair)
		}
		out[key.Operation][class] = append(out[key.Operation][class], Pair{From: key.From, To: key.To})
	}
	for op, classes := range out {
		for class := range classes {
			pairs := classes[class]
			sort.Slice(pairs, func(i, j int) bool {
				if pairs[i].From != pairs[j].From {
					return pairs[i].From < pairs[j].From
				}
				return pairs[i].To < pairs[j].To
			})
			classes[class] = pairs
		}
		out[op] = classes
	}
	return out
}

// Default is the process-wide registry populated by converters.go.
var Default = NewRegistry()
