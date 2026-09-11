import re

with open('/Users/dmemruk/development/services/apartment-hunter/internal/adapters/mcpagent/agent.go', 'r') as f:
    content = f.read()

# Extract ProcessCommand
start_idx = content.find("func (a *Agent) ProcessCommand(")
end_idx = content.find("func (a *Agent) makeChatRequest(")

process_command = content[start_idx:end_idx]

# Modify to ProcessFeedback
process_feedback = process_command.replace(
    "func (a *Agent) ProcessCommand(ctx context.Context, profileID string, text string)",
    "func (a *Agent) ProcessFeedback(ctx context.Context, profileID string, listing domain.ApartmentListing, feedbackText string)"
)

process_feedback = process_feedback.replace(
    "Ты - персональный ассистент по поиску квартир (MCP Agent). Твоя цель — помочь пользователю сформировать идеальные фильтры для поиска жилья.",
    "Ты - персональный ассистент по поиску квартир (MCP Agent). Пользователь ответил на отчет об объявлении о квартире с отзывом. Твоя цель — помочь пользователю уточнить фильтры, основываясь на его отзыве и данных объявления."
)

process_feedback = process_feedback.replace(
    "messages = append(messages, chatMessage{Role: \"user\", Content: text})",
    """listingJSON, _ := json.MarshalIndent(listing, "", "  ")
	userContent := fmt.Sprintf("Контекст: Я ответил на следующее объявление:\\n%s\\n\\nМой отзыв: %s", string(listingJSON), feedbackText)
	messages = append(messages, chatMessage{Role: "user", Content: userContent})"""
)

process_feedback = process_feedback.replace(
    "profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: \"user\", Content: text, CreatedAt: now})",
    "profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: \"user\", Content: \"[Отзыв на объявление] \" + feedbackText, CreatedAt: now})"
)

# Insert it right before makeChatRequest
new_content = content[:end_idx] + process_feedback + content[end_idx:]

with open('/Users/dmemruk/development/services/apartment-hunter/internal/adapters/mcpagent/agent.go', 'w') as f:
    f.write(new_content)
