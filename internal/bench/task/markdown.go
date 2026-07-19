package task

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/iodesystems/agentkit/llm"
)

// Markdown probes: a user-authorable alternative to task.yaml.
//
// The format exists so someone can add a probe without learning the YAML
// schema, and so a probe reads as documentation of what it tests. It maps
// LOSSLESSLY onto Task — there is no second runner, no second check
// vocabulary, and no behavior a markdown probe can express that a task.yaml
// cannot. That equivalence is the point: growing a parallel assertion language
// beside the existing one is exactly the duplication that folding crucible into
// this repo was meant to avoid.
//
//	---
//	name: vision-red-circle
//	class: capability
//	requires: { modality: image }
//	---
//
//	# Vision: a solid red circle
//
//	Prose here documents WHY the probe exists. Ignored by the runner.
//
//	## Prompt
//
//	What shape and colour is in this image? Answer briefly.
//
//	![a red circle](fixture/circle.png)
//
//	## Checks
//
//	- response_contains: red
//	- response_contains: circle
//
// Frontmatter carries everything that is not a stage (the same fields
// task.yaml uses). Each `## Prompt` opens a stage; a following `## Checks`
// list supplies that stage's checks; `## Options` sets per-stage flags.
// Stages run in document order in ONE session, exactly as in task.yaml.
//
// Check items are parsed as YAML and handed to the SAME Check.UnmarshalYAML
// that task.yaml uses, so every existing check kind works here for free and a
// new kind propagates to both formats at once.
const ProbeFile = "probe.md"

var (
	// fenceRe matches the leading YAML frontmatter block.
	fenceRe = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n?`)
	// imageRe matches a markdown image: ![alt](path). Only local paths are
	// resolved; an http(s) URL is passed through verbatim so a probe can point
	// at a hosted asset.
	imageRe = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)\)`)
	// headingRe matches a level-2 section heading.
	headingRe = regexp.MustCompile(`(?m)^##\s+(.+?)\s*$`)
)

// probeFrontmatter is the Task subset a markdown probe declares up top. It is
// deliberately the same field set as task.yaml rather than a reduced one — a
// markdown probe that outgrows the format should not have to be rewritten.
type probeFrontmatter struct {
	Name          string       `yaml:"name"`
	Class         string       `yaml:"class"`
	Description   string       `yaml:"description"`
	Workspace     string       `yaml:"workspace"`
	Limits        Limits       `yaml:"limits"`
	BaitTools     []BaitTool   `yaml:"baitTools"`
	Poison        []PoisonRule `yaml:"poison"`
	System        string       `yaml:"system"`
	SystemAppend  string       `yaml:"systemAppend"`
	ContextBudget int          `yaml:"contextBudget"`
	Run           string       `yaml:"run"`
	Requires      Requires     `yaml:"requires"`
	Audio         *AudioProbe  `yaml:"audio"`
}

// LoadMarkdown reads and validates <dir>/probe.md.
func LoadMarkdown(dir string) (*Task, error) {
	path := filepath.Join(dir, ProbeFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := parseMarkdown(string(b), dir)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	t.Dir = abs
	applyDefaults(t)
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

func parseMarkdown(src, dir string) (*Task, error) {
	var t Task

	m := fenceRe.FindStringSubmatch(src)
	if m == nil {
		return nil, fmt.Errorf("missing YAML frontmatter (a probe must open with a --- fenced block)")
	}
	var fm probeFrontmatter
	if err := yaml.Unmarshal([]byte(m[1]), &fm); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	t.Name, t.Class, t.Description = fm.Name, fm.Class, fm.Description
	t.Workspace, t.Limits, t.BaitTools, t.Poison = fm.Workspace, fm.Limits, fm.BaitTools, fm.Poison
	t.System, t.SystemAppend, t.ContextBudget = fm.System, fm.SystemAppend, fm.ContextBudget
	t.Run, t.Requires, t.Audio = fm.Run, fm.Requires, fm.Audio

	body := src[len(m[0]):]

	// Split the body on ## headings. Everything before the first heading is
	// free prose: it documents the probe and is shown in the UI, never sent to
	// the model. Falling back to it as a prompt would silently turn a comment
	// into an instruction.
	idx := headingRe.FindAllStringSubmatchIndex(body, -1)
	if len(idx) == 0 {
		return nil, fmt.Errorf("no '## Prompt' section found")
	}
	if t.Description == "" {
		t.Description = strings.TrimSpace(stripH1(body[:idx[0][0]]))
	}

	var cur *Stage
	for i, loc := range idx {
		title := strings.ToLower(strings.TrimSpace(body[loc[2]:loc[3]]))
		end := len(body)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		section := strings.TrimSpace(body[loc[1]:end])

		switch {
		case title == "prompt" || strings.HasPrefix(title, "prompt:"):
			text, parts, err := extractImages(section, dir)
			if err != nil {
				return nil, err
			}
			t.Stages = append(t.Stages, Stage{Prompt: text, Parts: parts})
			cur = &t.Stages[len(t.Stages)-1]
		case title == "checks":
			if cur == nil {
				return nil, fmt.Errorf("'## Checks' appears before any '## Prompt'")
			}
			cks, err := parseChecks(section)
			if err != nil {
				return nil, err
			}
			cur.Checks = cks
		case title == "options":
			if cur == nil {
				return nil, fmt.Errorf("'## Options' appears before any '## Prompt'")
			}
			var o struct {
				ForceCompact bool `yaml:"forceCompact"`
			}
			if err := yaml.Unmarshal([]byte(section), &o); err != nil {
				return nil, fmt.Errorf("options: %w", err)
			}
			cur.ForceCompact = o.ForceCompact
		default:
			// Unknown sections are prose. A probe is also documentation, so
			// "## Notes" or "## Why" must not be an error.
		}
	}
	if len(t.Stages) == 0 {
		return nil, fmt.Errorf("no '## Prompt' section found")
	}
	return &t, nil
}

// parseChecks decodes a markdown bullet list of checks by feeding it to the
// YAML decoder verbatim — each `- kind: value` item is already valid YAML for
// Check.UnmarshalYAML. Lines that are not list items are ignored so a section
// can carry an explanatory sentence above its list.
func parseChecks(section string) ([]Check, error) {
	// Keep each `- ` item AND the indented lines that belong to it. Discarding
	// continuations silently truncated every block scalar: `- python: |` kept
	// its first line and dropped the script, which surfaced as the baffling
	// "python: script is required" on a check whose script was plainly there.
	var items []string
	inItem := false
	itemIndent := 0
	for _, ln := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(ln)
		indent := len(ln) - len(strings.TrimLeft(ln, " \t"))
		switch {
		case strings.HasPrefix(trimmed, "- "):
			items = append(items, strings.TrimSpace(ln))
			inItem, itemIndent = true, indent
		case inItem && trimmed == "":
			// A blank line inside a block scalar is content, not a terminator.
			items = append(items, "")
		case inItem && indent > itemIndent:
			// Re-indent relative to the item so the YAML decoder sees a
			// consistent block, whatever the markdown nesting was.
			items = append(items, "  "+strings.TrimRight(ln[min(indent, itemIndent+2):], " "))
		default:
			inItem = false
		}
	}
	// Trailing blanks would end the document mid-scalar.
	for len(items) > 0 && strings.TrimSpace(items[len(items)-1]) == "" {
		items = items[:len(items)-1]
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("'## Checks' section has no `- ` list items")
	}
	var cks []Check
	if err := yaml.Unmarshal([]byte(strings.Join(items, "\n")), &cks); err != nil {
		return nil, fmt.Errorf("checks: %w", err)
	}
	return cks, nil
}

// extractImages pulls markdown images out of a prompt body and turns them into
// multimodal content parts. The returned text is the prompt with the image
// syntax removed; Parts is nil when the prompt has no images, which keeps a
// text-only probe byte-identical to a task.yaml one.
//
// Local paths resolve relative to the probe directory and are inlined as base64
// data URIs — the model backend cannot read the bench host's filesystem, so a
// path reference would silently send nothing.
func extractImages(section, dir string) (string, []llm.ContentPart, error) {
	locs := imageRe.FindAllStringSubmatchIndex(section, -1)
	if len(locs) == 0 {
		return section, nil, nil
	}
	var parts []llm.ContentPart
	for _, loc := range locs {
		ref := section[loc[4]:loc[5]]
		url := ref
		if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") && !strings.HasPrefix(ref, "data:") {
			b, err := os.ReadFile(filepath.Join(dir, ref))
			if err != nil {
				return "", nil, fmt.Errorf("image %q: %w", ref, err)
			}
			mime := mimeOf(ref)
			if mime == "" {
				return "", nil, fmt.Errorf("image %q: unsupported extension (want .png/.jpg/.jpeg/.gif/.webp)", ref)
			}
			url = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
		}
		parts = append(parts, llm.ContentPart{Type: "image_url", ImageURL: &llm.ImageURL{URL: url}})
	}
	text := strings.TrimSpace(imageRe.ReplaceAllString(section, ""))
	// The text part leads: providers expect the instruction alongside the
	// image, and a parts array with no text would drop the question entirely.
	return text, append([]llm.ContentPart{{Type: "text", Text: text}}, parts...), nil
}

func mimeOf(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// stripH1 drops a leading `# Title` line from the description prose — the
// title is display chrome, not part of the description.
func stripH1(s string) string {
	out := []string{}
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "# ") {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
