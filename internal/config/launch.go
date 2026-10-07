package config

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Launch is what a model's cmd says about how it runs: the weights, their
// quant, the KV cache types and the context it asks for. Read from the cmd
// text, so it is known before the model has ever been loaded — and it is what
// was CONFIGURED: the context the backend actually got is the probe's or the
// residency's n_ctx, which can be smaller.
//
// Every field is "" / 0 when the cmd does not say. KV types are left empty
// when unset rather than filled with llama.cpp's default (f16): the cmd may
// not be llama-server at all. Llama reports whether it is.
type Launch struct {
	// ModelFile is the weights file: -m/--model, else --hf-file, else the
	// -hf/--hf-repo repo (with its :TAG when given).
	ModelFile string
	// Quant is read from ModelFile's name (Q6_K_S, Q8_0, IQ4_XS, BF16 ...), or
	// from an -hf repo's :TAG.
	Quant string
	// CacheK / CacheV are --cache-type-k/-ctk and --cache-type-v/-ctv.
	CacheK string
	CacheV string
	// Ctx is -c/--ctx-size. 0 = not given (llama-server then uses the model's
	// trained context).
	Ctx int
	// Llama reports that the cmd runs llama-server, so unset KV types are
	// llama.cpp's default, f16.
	Llama bool
}

// quantRe matches a GGUF quant label at a name-token boundary. Longest forms
// first: IQ4_XS before Q4, Q6_K_S before Q6_K, UD-Q6_K_XL keeps its UD- prefix.
var quantRe = regexp.MustCompile(`(?i)(?:^|[-_.:])((?:UD-)?(?:IQ\d_[A-Z]+|Q\d_K_[SMLX]+|Q\d_K|Q\d_\d|TQ\d_\d|MXFP4|NVFP4|BF16|F16|F32))(?:$|[-_.])`)

// ParseLaunch reads a cmd as a shell would see its words: comments dropped,
// backslash-newline continuations joined, simple quotes removed. It does not
// expand ${tool:x} or variables — it needs only the flags' literal values.
func ParseLaunch(cmd string) Launch {
	var words []string
	for line := range strings.SplitSeq(strings.ReplaceAll(cmd, "\\\n", " "), "\n") {
		if i := commentStart(line); i >= 0 {
			line = line[:i]
		}
		for w := range strings.FieldsSeq(line) {
			words = append(words, strings.Trim(w, `"'`))
		}
	}

	var l Launch
	var hfRepo, hfFile string
	for i, w := range words {
		if strings.HasSuffix(w, "/llama-server") || w == "llama-server" {
			l.Llama = true
		}
		flag, val, inline := strings.Cut(w, "=")
		next := func() string {
			if inline {
				return val
			}
			if i+1 < len(words) {
				return words[i+1]
			}
			return ""
		}
		switch flag {
		case "-m", "--model":
			l.ModelFile = next()
		case "--hf-file", "-hff":
			hfFile = next()
		case "-hf", "-hfr", "--hf-repo":
			hfRepo = next()
		case "-ctk", "--cache-type-k":
			l.CacheK = strings.ToLower(next())
		case "-ctv", "--cache-type-v":
			l.CacheV = strings.ToLower(next())
		case "-c", "--ctx-size":
			if n, err := strconv.Atoi(next()); err == nil {
				l.Ctx = n
			}
		}
	}

	switch {
	case l.ModelFile != "":
	case hfFile != "":
		l.ModelFile = hfFile
	case hfRepo != "":
		l.ModelFile = hfRepo
	}
	l.Quant = QuantOf(l.ModelFile)
	if l.Quant == "" && hfRepo != "" {
		// -hf org/repo:Q6_K_XL names the quant as the tag.
		if _, tag, ok := strings.Cut(hfRepo, ":"); ok {
			l.Quant = strings.ToUpper(tag)
		}
	}
	return l
}

// QuantOf reads the quant label from a weights file name or path: the LAST
// match in the base name, so "Qwen3-Q8_0-draft-Q4_K_M.gguf" is Q4_K_M.
func QuantOf(file string) string {
	base := strings.TrimSuffix(path.Base(file), ".gguf")
	if base == "." || base == "" {
		return ""
	}
	var last string
	for rest := base; ; {
		m := quantRe.FindStringSubmatchIndex(rest)
		if m == nil {
			break
		}
		last = rest[m[2]:m[3]]
		rest = rest[m[3]:]
	}
	return strings.ToUpper(last)
}

// commentStart is where a # comment begins in a line: at the start or after
// whitespace, as in sh. -1 when there is none.
func commentStart(line string) int {
	for i, r := range line {
		if r == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return i
		}
	}
	return -1
}
