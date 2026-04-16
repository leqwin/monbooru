package metadata

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/leqwin/monbooru/internal/models"
)

// sanitizeComfyJSON replaces Python-specific non-standard JSON values (NaN,
// Infinity, -Infinity) with null so Go's JSON parser can handle ComfyUI's
// output. The previous implementation used strings.ReplaceAll and would
// mangle these substrings inside quoted string values (e.g. a prompt like
// "foo:Infinity bar" would become "foo:null bar"); we now walk the input
// token by token, tracking whether we are inside a string, and only touch
// unquoted regions.
func sanitizeComfyJSON(s string) string {
	// Quick check to avoid allocation when not needed.
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
		// Outside a string: try to match -Infinity / Infinity / NaN at this
		// position. These literals only ever appear as JSON number stand-ins,
		// so a match here is always safe to rewrite to null.
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

// parseComfyPromptChunk parses the ComfyUI API "prompt" chunk (dict format with class_type).
// This is the primary format used by ComfyUI when saving images.
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

	// Parse all nodes into a map for reference resolution
	parsedNodes := make(map[string]comfyAPINode, len(apiNodes))
	for id, nodeRaw := range apiNodes {
		var node comfyAPINode
		if err := json.Unmarshal(nodeRaw, &node); err == nil {
			parsedNodes[id] = node
		}
	}

	meta := &models.ComfyUIMetadata{}

	// First pass: find KSampler to resolve which CLIPTextEncode feeds the positive prompt.
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

	// Second pass: apply all node inputs
	for id, node := range parsedNodes {
		nodeType := node.ClassType
		if nodeType == "" {
			nodeType = node.Type
		}
		// For CLIPTextEncode: only use it as prompt if it's the positive reference
		if nodeType == "CLIPTextEncode" {
			if textRaw, ok := node.Inputs["text"]; ok {
				text := resolveStringInput(textRaw, parsedNodes)
				if text != "" {
					if positiveNodeID != "" {
						if id == positiveNodeID {
							meta.Prompt = text
						}
					} else if meta.Prompt == "" {
						// No KSampler reference found; pick first non-empty text
						meta.Prompt = text
					}
				}
			}
			continue
		}
		applyComfyNodeInputs(nodeType, node.Inputs, meta, parsedNodes)
	}

	// Fallback: PrimitiveStringMultiline nodes as prompt sources
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

// parseComfyWorkflow extracts ComfyUI metadata from the workflow JSON (node graph format).
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

	// New ComfyUI workflow format has "nodes" array at top level
	if nodesRaw, ok := workflow["nodes"]; ok {
		parseComfyNodesArray(nodesRaw, meta)
	} else {
		// Old flat dict format
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

// resolveStringInput tries to unmarshal raw as a string. If it's a reference
// array [nodeID, slotIndex], it follows the reference into parsedNodes looking
// for a string "value" input. Returns "" if nothing can be resolved.
func resolveStringInput(raw json.RawMessage, nodes map[string]comfyAPINode) string {
	// Try direct string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try reference array
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
	// Try to get a "value", "text", or "string" input from the referenced node
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

// resolveInt64Input tries to unmarshal raw as an int64. If it's a reference
// array, it follows the reference to find a numeric "seed" or "value" input.
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

// ParseComfyWorkflowNodes parses the raw_workflow JSON and returns a structured
// list of nodes suitable for display — one entry per node, with scalar inputs shown
// as values and array (reference) inputs shown as "→ nodeKey". Nodes are sorted
// by their string key (alphabetical). Nodes with no displayable inputs are included
// to give a complete picture of the workflow graph.
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
		Meta      struct{ Title string }    `json:"_meta"`
		Inputs    map[string]json.RawMessage `json:"inputs"`
	}

	// Collect and sort keys for stable output
	keys := make([]string, 0, len(apiNodes))
	parsed := make(map[string]rawNode, len(apiNodes))
	for k, v := range apiNodes {
		var n rawNode
		if err := json.Unmarshal(v, &n); err == nil {
			parsed[k] = n
			keys = append(keys, k)
		}
	}
	// Sort keys: numeric-looking keys first by value, then lexicographic
	sortComfyKeys(keys)

	nodes := make([]models.ComfyNode, 0, len(keys))
	for _, k := range keys {
		n := parsed[k]
		title := n.Meta.Title
		if title == "" {
			title = n.ClassType
		}

		var params []models.ComfyNodeParam
		// Sort input param names for stable output
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

// comfyParamToDisplay converts a raw JSON input value to a displayable ComfyNodeParam.
// Returns nil for null values.
func comfyParamToDisplay(name string, raw json.RawMessage) *models.ComfyNodeParam {
	if string(raw) == "null" {
		return nil
	}
	// Try as a string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return &models.ComfyNodeParam{Name: name, Value: s}
	}
	// Try as a number (float covers int)
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
	// Try as a bool
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		val := "false"
		if b {
			val = "true"
		}
		return &models.ComfyNodeParam{Name: name, Value: val}
	}
	// Try as an array — likely a reference [nodeKey, slotIndex]
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) >= 1 {
		var ref string
		if err := json.Unmarshal(arr[0], &ref); err == nil {
			return &models.ComfyNodeParam{Name: name, Value: "→ " + ref, IsRef: true}
		}
	}
	// Fallback: show raw JSON as-is; the detail template wraps long values
	// in a <details> toggle.
	return &models.ComfyNodeParam{Name: name, Value: string(raw)}
}

// sortComfyKeys sorts node keys: pure-integer keys numerically first, then the rest lexicographically.
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
		// Append LoRA info to model checkpoint
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
		// Multi-lora loader — collect non-None loras
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
