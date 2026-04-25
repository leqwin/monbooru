package metadata

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/leqwin/monbooru/internal/models"
)

// sanitizeComfyJSON replaces Python-only JSON literals (NaN, Infinity,
// -Infinity) with null so encoding/json can parse them. The walk tracks
// string state so that quoted text like "foo:Infinity bar" stays as-is;
// only unquoted regions are rewritten.
func sanitizeComfyJSON(s string) string {
	if !strings.Contains(s, "NaN") && !strings.Contains(s, "Infinity") {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			b.WriteByte(c)
			if escape {
				escape = false
				continue
			}
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}
		// Outside a string the three literals are always number stand-ins;
		// rewrite to null.
		switch {
		case strings.HasPrefix(s[i:], "-Infinity"):
			b.WriteString("null")
			i += len("-Infinity") - 1
		case strings.HasPrefix(s[i:], "Infinity"):
			b.WriteString("null")
			i += len("Infinity") - 1
		case strings.HasPrefix(s[i:], "NaN"):
			b.WriteString("null")
			i += len("NaN") - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// parseComfyPromptChunk parses the ComfyUI API "prompt" chunk (dict
// keyed by node id, with class_type per node). This is the primary
// format ComfyUI saves into images.
func parseComfyPromptChunk(raw string) *models.ComfyUIMetadata {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = sanitizeComfyJSON(raw)

	// API format: {"1": {"inputs": {...}, "class_type": "...", "_meta": {...}}, ...}
	var apiNodes map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &apiNodes); err != nil {
		return nil
	}

	parsedNodes := make(map[string]comfyAPINode, len(apiNodes))
	for id, nodeRaw := range apiNodes {
		var node comfyAPINode
		if err := json.Unmarshal(nodeRaw, &node); err == nil {
			parsedNodes[id] = node
		}
	}

	meta := &models.ComfyUIMetadata{}

	// First pass: find KSampler to learn which CLIPTextEncode is the
	// positive prompt.
	positiveNodeID := ""
	for _, node := range parsedNodes {
		nodeType := node.ClassType
		if nodeType == "" {
			nodeType = node.Type
		}
		if nodeType == "KSampler" || nodeType == "KSamplerAdvanced" {
			if posRaw, ok := node.Inputs["positive"]; ok {
				var ref []json.RawMessage
				if err := json.Unmarshal(posRaw, &ref); err == nil && len(ref) >= 1 {
					var nid string
					if err := json.Unmarshal(ref[0], &nid); err == nil {
						positiveNodeID = nid
					}
				}
			}
			break
		}
	}

	// Second pass: apply node inputs.
	for id, node := range parsedNodes {
		nodeType := node.ClassType
		if nodeType == "" {
			nodeType = node.Type
		}
		// For CLIPTextEncode, only use it as the prompt when it's the
		// positive reference; otherwise fall back to the first non-empty
		// text when no KSampler reference was found.
		if nodeType == "CLIPTextEncode" {
			if textRaw, ok := node.Inputs["text"]; ok {
				text := resolveStringInput(textRaw, parsedNodes)
				if text != "" {
					if positiveNodeID != "" {
						if id == positiveNodeID {
							meta.Prompt = text
						}
					} else if meta.Prompt == "" {
						meta.Prompt = text
					}
				}
			}
			continue
		}
		applyComfyNodeInputs(nodeType, node.Inputs, meta, parsedNodes)
	}

	// Fallback: PrimitiveStringMultiline as prompt source.
	if meta.Prompt == "" {
		for _, node := range parsedNodes {
			nodeType := node.ClassType
			if nodeType == "" {
				nodeType = node.Type
			}
			if nodeType == "PrimitiveStringMultiline" {
				if valRaw, ok := node.Inputs["value"]; ok {
					var val string
					if err := json.Unmarshal(valRaw, &val); err == nil && val != "" && len(val) > 10 {
						meta.Prompt = val
						break
					}
				}
			}
		}
	}

	if meta.Prompt == "" && meta.ModelCheckpoint == "" && meta.Seed == nil {
		return nil
	}
	meta.RawWorkflow = raw
	meta.GenerationHash = computeGenerationHash(
		meta.Prompt, "", meta.ModelCheckpoint, meta.Sampler, meta.Steps, meta.CFGScale,
	)
	return meta
}

// parseComfyWorkflow extracts ComfyUI metadata from the node-graph
// "workflow" JSON. Both old (flat dict) and new (top-level "nodes"
// array) formats are accepted.
func parseComfyWorkflow(raw string) *models.ComfyUIMetadata {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = sanitizeComfyJSON(raw)

	var workflow map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &workflow); err != nil {
		return nil
	}

	meta := &models.ComfyUIMetadata{RawWorkflow: raw}

	if nodesRaw, ok := workflow["nodes"]; ok {
		parseComfyNodesArray(nodesRaw, meta)
	} else {
		parseComfyNodesDict(workflow, meta)
	}

	if meta.Prompt == "" && meta.ModelCheckpoint == "" && meta.Seed == nil {
		return nil
	}
	meta.GenerationHash = computeGenerationHash(
		meta.Prompt, "", meta.ModelCheckpoint, meta.Sampler, meta.Steps, meta.CFGScale,
	)
	return meta
}

// comfyAPINode is the format used in the "prompt" chunk (API format).
type comfyAPINode struct {
	ClassType string                     `json:"class_type"`
	Type      string                     `json:"type"`
	Inputs    map[string]json.RawMessage `json:"inputs"`
}

// comfyNode is the format used in the "workflow" chunk (node graph format).
type comfyNode struct {
	Type   string                     `json:"type"`
	Inputs map[string]json.RawMessage `json:"inputs"`
}

// resolveStringInput unmarshals raw as a string, or follows a reference
// array [nodeID, slotIndex] to a "value" / "text" / "string" input on
// the referenced node. Returns "" when nothing resolves.
func resolveStringInput(raw json.RawMessage, nodes map[string]comfyAPINode) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var ref []json.RawMessage
	if err := json.Unmarshal(raw, &ref); err != nil || len(ref) < 1 {
		return ""
	}
	var nodeID string
	if err := json.Unmarshal(ref[0], &nodeID); err != nil {
		return ""
	}
	refNode, ok := nodes[nodeID]
	if !ok {
		return ""
	}
	for _, key := range []string{"value", "text", "string"} {
		if valRaw, ok := refNode.Inputs[key]; ok {
			var val string
			if err := json.Unmarshal(valRaw, &val); err == nil && val != "" {
				return val
			}
		}
	}
	return ""
}

// resolveInt64Input unmarshals raw as an int64, or follows a reference
// array to a numeric "seed" / "value" / "int" input on the referenced
// node.
func resolveInt64Input(raw json.RawMessage, nodes map[string]comfyAPINode) (int64, bool) {
	var v int64
	if err := json.Unmarshal(raw, &v); err == nil {
		return v, true
	}
	var ref []json.RawMessage
	if err := json.Unmarshal(raw, &ref); err != nil || len(ref) < 1 {
		return 0, false
	}
	var nodeID string
	if err := json.Unmarshal(ref[0], &nodeID); err != nil {
		return 0, false
	}
	refNode, ok := nodes[nodeID]
	if !ok {
		return 0, false
	}
	for _, key := range []string{"seed", "value", "int"} {
		if valRaw, ok := refNode.Inputs[key]; ok {
			if err := json.Unmarshal(valRaw, &v); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

func parseComfyNodesArray(raw json.RawMessage, meta *models.ComfyUIMetadata) {
	var nodes []comfyNode
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return
	}
	for _, node := range nodes {
		applyComfyNodeInputs(node.Type, node.Inputs, meta, nil)
	}
}

func parseComfyNodesDict(workflow map[string]json.RawMessage, meta *models.ComfyUIMetadata) {
	for _, nodeRaw := range workflow {
		var node comfyNode
		if err := json.Unmarshal(nodeRaw, &node); err != nil {
			continue
		}
		applyComfyNodeInputs(node.Type, node.Inputs, meta, nil)
	}
}

// ParseComfyWorkflowNodes parses the raw_workflow JSON for display.
// Each entry is one node; scalar inputs render as values, array
// (reference) inputs as "→ nodeKey". Nodes with no inputs are kept so
// the graph stays complete. Output is sorted: numeric keys first (by
// value), then lexicographic.
func ParseComfyWorkflowNodes(raw string) []models.ComfyNode {
	if raw == "" {
		return nil
	}
	raw = sanitizeComfyJSON(raw)

	// Parse top-level as map of nodeKey → raw node JSON
	var apiNodes map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &apiNodes); err != nil {
		return nil
	}

	type rawNode struct {
		ClassType string                     `json:"class_type"`
		Meta      struct{ Title string }     `json:"_meta"`
		Inputs    map[string]json.RawMessage `json:"inputs"`
	}

	keys := make([]string, 0, len(apiNodes))
	parsed := make(map[string]rawNode, len(apiNodes))
	for k, v := range apiNodes {
		var n rawNode
		if err := json.Unmarshal(v, &n); err == nil {
			parsed[k] = n
			keys = append(keys, k)
		}
	}
	sortComfyKeys(keys)

	nodes := make([]models.ComfyNode, 0, len(keys))
	for _, k := range keys {
		n := parsed[k]
		title := n.Meta.Title
		if title == "" {
			title = n.ClassType
		}

		var params []models.ComfyNodeParam
		paramKeys := make([]string, 0, len(n.Inputs))
		for pk := range n.Inputs {
			paramKeys = append(paramKeys, pk)
		}
		sortStringSlice(paramKeys)

		for _, pk := range paramKeys {
			raw := n.Inputs[pk]
			param := comfyParamToDisplay(pk, raw)
			if param != nil {
				params = append(params, *param)
			}
		}

		nodes = append(nodes, models.ComfyNode{
			Key:       k,
			Title:     title,
			ClassType: n.ClassType,
			Params:    params,
		})
	}
	return nodes
}

// comfyParamToDisplay converts a raw JSON input value into a
// ComfyNodeParam suitable for display. Returns nil for JSON null.
func comfyParamToDisplay(name string, raw json.RawMessage) *models.ComfyNodeParam {
	if string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return &models.ComfyNodeParam{Name: name, Value: s}
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		var val string
		if f == float64(int64(f)) {
			val = fmt.Sprintf("%d", int64(f))
		} else {
			val = fmt.Sprintf("%g", f)
		}
		return &models.ComfyNodeParam{Name: name, Value: val}
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		val := "false"
		if b {
			val = "true"
		}
		return &models.ComfyNodeParam{Name: name, Value: val}
	}
	// Array → reference [nodeKey, slotIndex].
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) >= 1 {
		var ref string
		if err := json.Unmarshal(arr[0], &ref); err == nil {
			return &models.ComfyNodeParam{Name: name, Value: "→ " + ref, IsRef: true}
		}
	}
	// Anything else: raw JSON; the template wraps long values in a
	// <details> toggle.
	return &models.ComfyNodeParam{Name: name, Value: string(raw)}
}

// sortComfyKeys sorts pure-integer keys numerically first, then the
// rest lexicographically.
func sortComfyKeys(keys []string) {
	slices.SortFunc(keys, func(a, b string) int {
		ai, bi := parseIntKey(a), parseIntKey(b)
		if ai >= 0 && bi >= 0 {
			return cmp.Compare(ai, bi)
		}
		if ai >= 0 {
			return -1
		}
		if bi >= 0 {
			return 1
		}
		return cmp.Compare(a, b)
	})
}

func parseIntKey(s string) int {
	if s == "" {
		return -1
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func sortStringSlice(ss []string) {
	slices.Sort(ss)
}

func applyComfyNodeInputs(nodeType string, inputs map[string]json.RawMessage, meta *models.ComfyUIMetadata, nodes map[string]comfyAPINode) {
	switch nodeType {
	case "CLIPTextEncode":
		if textRaw, ok := inputs["text"]; ok && meta.Prompt == "" {
			// text can be a string literal or a reference array [nodeID, slotIndex]
			text := resolveStringInput(textRaw, nodes)
			if text != "" {
				meta.Prompt = text
			}
		}
	case "KSampler", "KSamplerAdvanced", "KSamplerSelect":
		if seedRaw, ok := inputs["seed"]; ok && meta.Seed == nil {
			if seed, ok := resolveInt64Input(seedRaw, nodes); ok {
				meta.Seed = &seed
			}
		}
		if stepsRaw, ok := inputs["steps"]; ok && meta.Steps == nil {
			var steps int
			if err := json.Unmarshal(stepsRaw, &steps); err == nil {
				meta.Steps = &steps
			}
		}
		if cfgRaw, ok := inputs["cfg"]; ok && meta.CFGScale == nil {
			var cfg float64
			if err := json.Unmarshal(cfgRaw, &cfg); err == nil {
				meta.CFGScale = &cfg
			}
		}
		if samplerRaw, ok := inputs["sampler_name"]; ok && meta.Sampler == "" {
			var sampler string
			if err := json.Unmarshal(samplerRaw, &sampler); err == nil {
				meta.Sampler = sampler
			}
		}
		if schedulerRaw, ok := inputs["scheduler"]; ok && meta.Sampler != "" {
			var scheduler string
			if err := json.Unmarshal(schedulerRaw, &scheduler); err == nil && scheduler != "" {
				meta.Sampler += "/" + scheduler
			}
		}
	case "CheckpointLoaderSimple", "CheckpointLoader", "unCLIPCheckpointLoader":
		if ckptRaw, ok := inputs["ckpt_name"]; ok && meta.ModelCheckpoint == "" {
			var ckpt string
			if err := json.Unmarshal(ckptRaw, &ckpt); err == nil {
				meta.ModelCheckpoint = ckpt
			}
		}
	case "UNETLoader":
		// Flux/SDXL flux-style separate unet+clip+vae loading
		if unetRaw, ok := inputs["unet_name"]; ok && meta.ModelCheckpoint == "" {
			var unet string
			if err := json.Unmarshal(unetRaw, &unet); err == nil {
				meta.ModelCheckpoint = unet
			}
		}
	case "LoraLoader", "LoraLoaderModelOnly":
		if loraRaw, ok := inputs["lora_name"]; ok {
			var lora string
			if err := json.Unmarshal(loraRaw, &lora); err == nil && lora != "" {
				if meta.ModelCheckpoint != "" {
					meta.ModelCheckpoint += " + " + lora
				} else {
					meta.ModelCheckpoint = lora
				}
			}
		}
	case "Lora Loader Stack (rgthree)":
		// Multi-lora loader; collect every non-"None" slot.
		for i := 1; i <= 10; i++ {
			key := fmt.Sprintf("lora_%02d", i)
			if loraRaw, ok := inputs[key]; ok {
				var lora string
				if err := json.Unmarshal(loraRaw, &lora); err == nil && lora != "" && lora != "None" {
					if meta.ModelCheckpoint != "" {
						meta.ModelCheckpoint += " + " + lora
					} else {
						meta.ModelCheckpoint = lora
					}
				}
			}
		}
	case "Seed (rgthree)", "SeedNode", "RandomSeed":
		if seedRaw, ok := inputs["seed"]; ok && meta.Seed == nil {
			var seed int64
			if err := json.Unmarshal(seedRaw, &seed); err == nil {
				meta.Seed = &seed
			}
		}
	case "easy fullLoader", "easy a1111Loader":
		if ckptRaw, ok := inputs["ckpt_name"]; ok && meta.ModelCheckpoint == "" {
			var ckpt string
			if err := json.Unmarshal(ckptRaw, &ckpt); err == nil {
				meta.ModelCheckpoint = ckpt
			}
		}
	}
}
