package config

import "testing"

// The production cmd as configured on box1 (2026-10-07): comments, an export,
// ${tool:x}, continuations.
const swiftCmd = `# Swift-1.5-Qwen3.8-27B-Q6_K_S: Swift 1.5, ukisai Q6_K_S (P42).
# --spec-draft-sampling probabilistic needs llama.cpp >= 1fb7ef3e3
export CUDA_VISIBLE_DEVICES=GPU-ee90af07-0882-d325-182e-87137ec6d47b
# exec is REQUIRED: without it corrallm tracks this shell, not llama-server.
exec ${tool:llama.cpp}/llama-server \
  -m /home/nthalk/.corrallm/models/Swift-1.5-Qwen3.8-27B-Q6_K_S.gguf \
  --mmproj /home/nthalk/.corrallm/models/mmproj-Swift-1.5-Qwen3.8-27B-F16.gguf \
  -fa on --cache-type-k q8_0 --cache-type-v q5_1 \
  -c 188000 -ngl 99 --port 5806`

const nomicCmd = `CUDA_VISIBLE_DEVICES=GPU-76a4c775 \
${tool:llama.cpp}/llama-server \
  --embeddings \
  --ctx-size 8192 \
  -hf nomic-ai/nomic-embed-text-v1.5-GGUF \
  --hf-file nomic-embed-text-v1.5.Q4_K_M.gguf \
  --port 5801`

func TestParseLaunchProductionCmds(t *testing.T) {
	cases := []struct {
		name, cmd string
		want      Launch
	}{
		{"swift", swiftCmd, Launch{
			ModelFile: "/home/nthalk/.corrallm/models/Swift-1.5-Qwen3.8-27B-Q6_K_S.gguf",
			Quant:     "Q6_K_S", CacheK: "q8_0", CacheV: "q5_1", Ctx: 188000, Llama: true}},
		// The projector's F16 must not be read as the weights' quant.
		{"nomic via --hf-file", nomicCmd, Launch{
			ModelFile: "nomic-embed-text-v1.5.Q4_K_M.gguf", Quant: "Q4_K_M", Ctx: 8192, Llama: true}},
		{"hf repo tag", `llama-server -hf unsloth/Qwen3-GGUF:UD-Q6_K_XL -ctk q8_0 -ctv=q8_0`, Launch{
			ModelFile: "unsloth/Qwen3-GGUF:UD-Q6_K_XL", Quant: "UD-Q6_K_XL", CacheK: "q8_0", CacheV: "q8_0", Llama: true}},
		{"not llama", `python serve.py --port 9000`, Launch{}},
	}
	for _, c := range cases {
		if got := ParseLaunch(c.cmd); got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestQuantOf(t *testing.T) {
	for file, want := range map[string]string{
		"chandra-ocr-2.Q8_0.gguf":            "Q8_0",
		"Qwen3.8-27B-IQ4_XS.gguf":            "IQ4_XS",
		"model-bf16.gguf":                    "BF16",
		"Qwen3-Q8_0-draft-Q4_K_M.gguf":       "Q4_K_M",
		"/m/Swift-1.5-Qwen3.8-27B-Q6_K.gguf": "Q6_K",
		"gpt-oss-20b-MXFP4.gguf":             "MXFP4",
		"no-quant-here.gguf":                 "",
		"":                                   "",
	} {
		if got := QuantOf(file); got != want {
			t.Errorf("QuantOf(%q) = %q, want %q", file, got, want)
		}
	}
}
