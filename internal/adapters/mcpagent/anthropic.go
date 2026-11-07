package mcpagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

const anthropicMaxTokens = 16000

func (a *Agent) makeAnthropicRequest(ctx context.Context, reqBody chatRequest) (chatMessage, error) {
	system, messages := anthropicConversation(reqBody.Messages)
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(reqBody.Model),
		MaxTokens: anthropicMaxTokens,
		System:    system,
		Messages:  messages,
		Tools:     anthropicTools(reqBody.Tools),
	}
	if a.effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(a.effort)}
	}
	if reqBody.ToolChoice == "none" && len(params.Tools) > 0 {
		params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
	}

	resp, err := a.anthropic.Messages.New(ctx, params)
	if err != nil {
		return chatMessage{}, fmt.Errorf("anthropic messages api: %w", err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return chatMessage{}, fmt.Errorf("anthropic model refused the request")
	}
	if resp.StopReason == anthropic.StopReasonMaxTokens {
		return chatMessage{}, fmt.Errorf("anthropic response hit max_tokens")
	}

	reply := chatMessage{Role: "assistant", anthropicContent: resp.ToParam().Content}
	var text strings.Builder
	for _, block := range resp.Content {
		switch variant := block.AsAny().(type) {
		case anthropic.TextBlock:
			text.WriteString(variant.Text)
		case anthropic.ToolUseBlock:
			call := toolCall{ID: variant.ID, Type: "function"}
			call.Function.Name = variant.Name
			call.Function.Arguments = string(variant.Input)
			reply.ToolCalls = append(reply.ToolCalls, call)
		}
	}
	reply.Content = text.String()
	return reply, nil
}

func anthropicConversation(messages []chatMessage) ([]anthropic.TextBlockParam, []anthropic.MessageParam) {
	var system []anthropic.TextBlockParam
	var converted []anthropic.MessageParam
	var pendingToolResults []anthropic.ContentBlockParamUnion

	flushToolResults := func() {
		if len(pendingToolResults) > 0 {
			converted = append(converted, anthropic.NewUserMessage(pendingToolResults...))
			pendingToolResults = nil
		}
	}

	for _, msg := range messages {
		if msg.Role == "tool" {
			pendingToolResults = append(pendingToolResults, anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, false))
			continue
		}
		flushToolResults()
		switch msg.Role {
		case "system":
			if strings.TrimSpace(msg.Content) != "" {
				system = append(system, anthropic.TextBlockParam{Text: msg.Content})
			}
		case "assistant":
			if blocks := assistantBlocks(msg); len(blocks) > 0 {
				converted = append(converted, anthropic.NewAssistantMessage(blocks...))
			}
		default:
			if strings.TrimSpace(msg.Content) != "" {
				converted = append(converted, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
			}
		}
	}
	flushToolResults()
	return system, converted
}

func assistantBlocks(msg chatMessage) []anthropic.ContentBlockParamUnion {
	if len(msg.anthropicContent) > 0 {
		return msg.anthropicContent
	}
	var blocks []anthropic.ContentBlockParamUnion
	if strings.TrimSpace(msg.Content) != "" {
		blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
	}
	return blocks
}

func anthropicTools(tools []chatTool) []anthropic.ToolUnionParam {
	converted := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		schema := anthropic.ToolInputSchemaParam{ExtraFields: map[string]any{}}
		for key, value := range tool.Function.Parameters {
			switch key {
			case "type":
			case "properties":
				schema.Properties = value
			case "required":
				schema.Required = requiredFields(value)
			default:
				schema.ExtraFields[key] = value
			}
		}
		converted = append(converted, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        tool.Function.Name,
			Description: anthropic.String(tool.Function.Description),
			InputSchema: schema,
		}})
	}
	return converted
}

func requiredFields(value any) []string {
	switch fields := value.(type) {
	case []string:
		return fields
	case []any:
		names := make([]string, 0, len(fields))
		for _, field := range fields {
			if name, ok := field.(string); ok {
				names = append(names, name)
			}
		}
		return names
	}
	return nil
}
