package apicompat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const deepSeekToolTextLimit = 1 << 20

// DeepSeekToolTextDecoder separates reasoning and repairs a complete DSML tool
// block leaked into content. It is opt-in for DeepSeek requests, and accepts
// tool calls only when their names were declared in the request.
// Text before the first '<' streams normally; a possible markup suffix is held
// until EOF so truncated or ambiguous calls can never be completed for execution.
type DeepSeekToolTextDecoder struct {
	declared       map[string]bool
	prefix         strings.Builder
	pending        strings.Builder
	holding        bool
	native         bool
	overflow       bool
	toolChoiceNone bool
}

func NewDeepSeekToolTextDecoder(tools []ChatTool, toolChoice ...json.RawMessage) *DeepSeekToolTextDecoder {
	d := &DeepSeekToolTextDecoder{declared: make(map[string]bool)}
	if len(toolChoice) > 0 {
		var choice string
		if json.Unmarshal(toolChoice[0], &choice) == nil {
			d.toolChoiceNone = choice == "none"
		}
	}
	for _, tool := range tools {
		if tool.Type == "function" && tool.Function != nil {
			d.declared[tool.Function.Name] = true
		}
	}
	return d
}

// Push leaves native tool deltas and usage intact. It never mutates the caller's
// chunk, which may also be consumed by accounting or diagnostic code.
func (d *DeepSeekToolTextDecoder) Push(chunk *ChatCompletionsChunk) *ChatCompletionsChunk {
	out := *chunk
	out.Choices = append([]ChatChunkChoice(nil), chunk.Choices...)
	for i := range out.Choices {
		choice := &out.Choices[i]
		if choice.Index != 0 {
			continue
		}
		if len(choice.Delta.ToolCalls) > 0 {
			d.native = true
		}
		if choice.Delta.Content == nil {
			continue
		}
		text := *choice.Delta.Content
		if !d.holding {
			if idx := strings.IndexByte(text, '<'); idx >= 0 {
				d.holding = true
				_, _ = d.prefix.WriteString(text[:idx])
				d.appendPending(text[idx:])
				text = text[:idx]
			} else {
				_, _ = d.prefix.WriteString(text)
			}
		} else {
			d.appendPending(text)
			text = ""
		}
		choice.Delta.Content = &text
	}
	return &out
}

func (d *DeepSeekToolTextDecoder) appendPending(text string) {
	if d.overflow {
		return
	}
	if d.pending.Len()+len(text) > deepSeekToolTextLimit {
		d.overflow = true
		d.pending.Reset()
		return
	}
	_, _ = d.pending.WriteString(text)
}

// Finish returns only content not already emitted by Push, plus recovered calls.
// A native call alongside text markup is ambiguous and must not be duplicated.
func (d *DeepSeekToolTextDecoder) Finish(finishReason string) (*ChatCompletionsChunk, error) {
	switch finishReason {
	case "", "stop", "length", "content_filter", "tool_calls", "function_call":
	default:
		return nil, fmt.Errorf("DeepSeek returned an unsupported finish reason")
	}
	if d.overflow {
		return nil, fmt.Errorf("DeepSeek tool markup exceeded buffer limit")
	}
	pending := d.pending.String()
	pending, reasoning, err := d.recoverThinking(d.prefix.String(), pending)
	if err != nil {
		return nil, err
	}
	translatedThinking := pending != d.pending.String()
	text, calls, err := d.recoverText(d.prefix.String(), pending)
	if err != nil {
		return nil, err
	}
	if len(calls) > 0 && (d.native || d.toolChoiceNone || (finishReason != "stop" && finishReason != "tool_calls")) {
		return nil, fmt.Errorf("ambiguous or incomplete DeepSeek text tool calls")
	}
	if translatedThinking && strings.TrimSpace(d.prefix.String()+text) == "" && len(calls) == 0 && !d.native {
		return nil, fmt.Errorf("DeepSeek returned thinking without an answer or tool call")
	}
	if text == "" && reasoning == "" && len(calls) == 0 && !translatedThinking {
		return nil, nil
	}
	var reasoningContent *string
	if reasoning != "" {
		reasoningContent = &reasoning
	}
	reason := finishReason
	if len(calls) > 0 {
		reason = "tool_calls"
	}
	return &ChatCompletionsChunk{Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{Content: &text, ReasoningContent: reasoningContent, ToolCalls: calls}, FinishReason: &reason}}}, nil
}

// Only protocol blocks outside Markdown code examples are translated. The
// first possible delimiter has already been held by Push, including when UTF-8
// or tag bytes are split across upstream chunks.
func (d *DeepSeekToolTextDecoder) recoverThinking(prefix, pending string) (string, string, error) {
	all := prefix + pending
	var visible, reasoning strings.Builder
	var markdown deepSeekMarkdownContext
	cursor := 0
	for offset := 0; offset < len(all); {
		lineEnd := len(all)
		if newline := strings.IndexByte(all[offset:], '\n'); newline >= 0 {
			lineEnd = offset + newline + 1
		}
		line := all[offset:lineEnd]
		trim := strings.TrimLeft(line, " ")
		indent := len(line) - len(trim)
		code, outsideCode := markdown.scanLine(line, all[offset:])
		if !code && indent <= 3 {
			if strings.HasPrefix(trim, "<think>") {
				start := offset + indent
				contentStart := start + len("<think>")
				closeAt := strings.Index(all[contentStart:], "</think>")
				if closeAt < 0 {
					return "", "", fmt.Errorf("incomplete DeepSeek thinking block")
				}
				if strings.Contains(all[contentStart:contentStart+closeAt], "<think>") {
					return "", "", fmt.Errorf("nested DeepSeek thinking blocks")
				}
				_, _ = visible.WriteString(all[cursor:start])
				_, _ = reasoning.WriteString(all[contentStart : contentStart+closeAt])
				cursor = contentStart + closeAt + len("</think>")
				offset = cursor
				continue
			}
			if strings.HasPrefix(trim, "</think") || strings.HasPrefix(trim, "<think") ||
				(cursor > 0 && strings.Contains(outsideCode, "</think>")) {
				return "", "", fmt.Errorf("malformed DeepSeek thinking delimiter")
			}
			// Everything after a tool block belongs to the tool parser. In
			// particular, a command's literal <think> must stay in its argument.
			if strings.HasPrefix(trim, "<") && isDeepSeekToolMarkup(trim) {
				if len(d.declared) > 0 {
					break
				}
				if strings.Contains(strings.SplitN(trim, ">", 2)[0], "DSML") ||
					dsToolOpen.MatchString(trim) || dsToolClose.MatchString(trim) {
					return "", "", fmt.Errorf("DeepSeek returned tool markup without declared tools")
				}
			}
		}
		offset = lineEnd
	}
	_, _ = visible.WriteString(all[cursor:])
	return visible.String()[len(prefix):], reasoning.String(), nil
}

// deepSeekMarkdownContext keeps protocol examples literal, including fences
// nested in list items and backtick code spans that continue across lines.
type deepSeekMarkdownContext struct {
	fence          byte
	fenceLen       int
	containerWidth int
	spanLen        int
}

var deepSeekMarkdownList = regexp.MustCompile(`^(?:[-+*]|[0-9]{1,9}[.)])[ \t]+`)

func (m *deepSeekMarkdownContext) scanLine(line, remaining string) (bool, string) {
	trim := strings.TrimLeft(line, " ")
	indent := len(line) - len(trim)
	candidate := trim
	containerWidth := 0
	for {
		if strings.HasPrefix(candidate, ">") {
			candidate = strings.TrimLeft(candidate[1:], " \t")
		} else if marker := deepSeekMarkdownList.FindString(candidate); marker != "" {
			candidate = strings.TrimLeft(candidate[len(marker):], " \t")
		} else {
			break
		}
		containerWidth = len(line) - len(candidate)
	}
	if m.spanLen == 0 && (strings.HasPrefix(candidate, "```") || strings.HasPrefix(candidate, "~~~")) {
		ch := candidate[0]
		n := 0
		for n < len(candidate) && candidate[n] == ch {
			n++
		}
		if m.fence == 0 && indent <= 3 {
			m.fence, m.fenceLen, m.containerWidth = ch, n, containerWidth
			return true, ""
		}
		if m.fence == ch && n >= m.fenceLen && indent <= m.containerWidth+3 && strings.TrimSpace(candidate[n:]) == "" {
			m.fence, m.fenceLen, m.containerWidth = 0, 0, 0
			return true, ""
		}
	}
	if m.fence != 0 || indent >= 4 || strings.HasPrefix(trim, "\t") {
		return true, ""
	}
	// A real protocol block is parsed by the caller, so its reasoning or tool
	// argument must not change the surrounding Markdown state.
	if m.spanLen == 0 && (strings.HasPrefix(trim, "<think") || strings.HasPrefix(trim, "</think") ||
		(strings.HasPrefix(trim, "<") && isDeepSeekToolMarkup(trim))) {
		return false, line
	}
	startedInCode := m.spanLen != 0
	outside := []byte(line)
	for i := 0; i < len(line); {
		if line[i] != '`' || (m.spanLen == 0 && i > 0 && line[i-1] == '\\') {
			if m.spanLen != 0 {
				outside[i] = ' '
			}
			i++
			continue
		}
		end := i + 1
		for end < len(line) && line[end] == '`' {
			end++
		}
		runLen := end - i
		if m.spanLen == 0 {
			// An unmatched backtick is ordinary Markdown text. It must not
			// hide a real thinking/tool block later in the response.
			foundClose := false
			for rest := remaining[end:]; rest != ""; {
				next := strings.IndexByte(rest, '`')
				if next < 0 {
					break
				}
				rest = rest[next:]
				n := 1
				for n < len(rest) && rest[n] == '`' {
					n++
				}
				if n == runLen {
					foundClose = true
					break
				}
				rest = rest[n:]
			}
			if !foundClose {
				i = end
				continue
			}
			m.spanLen = runLen
		} else if runLen == m.spanLen {
			m.spanLen = 0
		}
		for ; i < end; i++ {
			outside[i] = ' '
		}
	}
	return startedInCode, string(outside)
}

// DSML values are raw strings, not XML escaped character data. Parse only the
// protocol delimiters instead of using an XML decoder (which corrupts '<' and
// '&' in command/code arguments). No unescaping or guessing missing names/types.
var (
	dsToolOpen    = regexp.MustCompile(`^<(?:｜DSML｜|\|DSML\|)?tool_calls>\s*`)
	dsToolClose   = regexp.MustCompile(`^</(?:｜DSML｜|\|DSML\|)?tool_calls>`)
	dsInvokeOpen  = regexp.MustCompile(`^<(?:｜DSML｜|\|DSML\|)?invoke name="([^"]+)">\s*`)
	dsInvokeClose = regexp.MustCompile(`^</(?:｜DSML｜|\|DSML\|)?invoke>\s*`)
	dsParamOpen   = regexp.MustCompile(`^<(?:｜DSML｜|\|DSML\|)?parameter name="([^"]+)" string="(true|false)">`)
	dsParamClose  = regexp.MustCompile(`</(?:｜DSML｜|\|DSML\|)?parameter>`)
)

func (d *DeepSeekToolTextDecoder) recoverText(prefix, pending string) (string, []ChatToolCall, error) {
	if len(d.declared) == 0 {
		return pending, nil, nil
	}
	// Code samples are text, never executable tools. Recognize only standalone
	// protocol lines outside Markdown fences, including orphan parameter fragments.
	var markdown deepSeekMarkdownContext
	offset := 0
	all := prefix + pending
	for _, line := range strings.SplitAfter(all, "\n") {
		trim := strings.TrimLeft(line, " ")
		// Four-space and tab-indented code blocks are examples, too.
		indent := len(line) - len(trim)
		code, _ := markdown.scanLine(line, all[offset:])
		if !code && indent <= 3 && strings.HasPrefix(trim, "<") && isDeepSeekToolMarkup(trim) {
			start := offset + strings.Index(line, "<") - len(prefix)
			if start < 0 {
				return "", nil, fmt.Errorf("invalid DeepSeek tool markup boundary")
			}
			calls, err := d.parseCalls(strings.TrimSpace(pending[start:]))
			if err != nil {
				return "", nil, err
			}
			return pending[:start], calls, nil
		}
		offset += len(line)
	}
	return pending, nil, nil
}

func isDeepSeekToolMarkup(text string) bool {
	if strings.Contains(strings.SplitN(text, ">", 2)[0], "DSML") {
		return true
	}
	for _, tag := range []string{"tool_calls", "invoke", "parameter"} {
		if strings.HasPrefix(text, "<"+tag+">") || strings.HasPrefix(text, "<"+tag+" ") || strings.HasPrefix(text, "</"+tag+">") {
			return true
		}
	}
	return false
}

func (d *DeepSeekToolTextDecoder) parseCalls(text string) ([]ChatToolCall, error) {
	invalid := func() ([]ChatToolCall, error) {
		return nil, fmt.Errorf("malformed or undeclared DeepSeek text tool call")
	}
	opening := dsToolOpen.FindString(text)
	if opening == "" {
		return invalid()
	}
	text = text[len(opening):]
	var calls []ChatToolCall
	for {
		if closing := dsToolClose.FindString(text); closing != "" {
			if len(calls) == 0 || strings.TrimSpace(text[len(closing):]) != "" {
				return invalid()
			}
			return calls, nil
		}
		invoke := dsInvokeOpen.FindStringSubmatch(text)
		if invoke == nil || !d.declared[invoke[1]] {
			return invalid()
		}
		name := invoke[1]
		text = text[len(invoke[0]):]
		args := make(map[string]json.RawMessage)
		for {
			if closing := dsInvokeClose.FindString(text); closing != "" {
				text = text[len(closing):]
				break
			}
			param := dsParamOpen.FindStringSubmatch(text)
			if param == nil || param[1] == "" {
				return invalid()
			}
			if _, duplicate := args[param[1]]; duplicate {
				return invalid()
			}
			text = text[len(param[0]):]
			closing := dsParamClose.FindStringIndex(text)
			if closing == nil {
				return invalid()
			}
			value := text[:closing[0]]
			if param[2] == "true" {
				encoded, _ := json.Marshal(value)
				args[param[1]] = encoded
			} else {
				if !json.Valid([]byte(value)) {
					return invalid()
				}
				args[param[1]] = json.RawMessage(value)
			}
			text = strings.TrimLeft(text[closing[1]:], " \t\r\n")
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			return invalid()
		}
		idx := len(calls)
		calls = append(calls, ChatToolCall{Index: &idx, ID: "call_" + generateItemID(), Type: "function", Function: ChatFunctionCall{Name: name, Arguments: string(encoded)}})
	}
}

// RecoverMessage applies the same policy to a non-streaming Chat response.
func (d *DeepSeekToolTextDecoder) RecoverMessage(message *ChatMessage, finishReason string) error {
	text := chatMessageContentText(message.Content)
	d.native = len(message.ToolCalls) > 0
	if strings.TrimSpace(text) == "" && strings.TrimSpace(message.reasoningText()) != "" && !d.native {
		return fmt.Errorf("DeepSeek returned reasoning without an answer or tool call")
	}
	d.appendPending(text)
	chunk, err := d.Finish(finishReason)
	if err != nil {
		return err
	}
	if chunk == nil {
		return nil
	}
	delta := chunk.Choices[0].Delta
	if *delta.Content != text || len(delta.ToolCalls) > 0 {
		message.Content, _ = json.Marshal(*delta.Content)
	}
	if delta.ReasoningContent != nil {
		message.ReasoningContent = message.reasoningText() + *delta.ReasoningContent
	}
	if len(delta.ToolCalls) > 0 {
		message.ToolCalls = delta.ToolCalls
	}
	return nil
}
