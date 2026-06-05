package interview

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"apartment-hunter/internal/adapters/geocodingmock"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/geocoding"
)

type QuestionKind string

const (
	QuestionString     QuestionKind = "string"
	QuestionInt        QuestionKind = "int"
	QuestionFloat      QuestionKind = "float"
	QuestionBool       QuestionKind = "bool"
	QuestionImportance QuestionKind = "importance"
)

type Question struct {
	ID     string
	Prompt string
	Kind   QuestionKind
}

type Session struct {
	prefs    domain.UserPreferences
	step     int
	distance int
	done     bool
	resolver geocoding.Resolver
}

func NewSession() *Session {
	return NewSessionWithResolver(geocodingmock.New())
}

func NewSessionWithResolver(resolver geocoding.Resolver) *Session {
	if resolver == nil {
		resolver = geocodingmock.New()
	}
	return &Session{resolver: resolver}
}

func Questions() []Question {
	return []Question{
		{ID: "min_budget", Prompt: "Минимальный бюджет в месяц? Напишите 0, если минимума нет", Kind: QuestionInt},
		{ID: "max_budget", Prompt: "Максимальный бюджет в месяц?", Kind: QuestionInt},
		{ID: "min_area_m2", Prompt: "Минимальная площадь, м2?", Kind: QuestionFloat},
		{ID: "require_dishwasher", Prompt: "Посудомойка обязательна? да/нет", Kind: QuestionBool},
		{ID: "require_oven", Prompt: "Духовка обязательна? да/нет", Kind: QuestionBool},
		{ID: "max_distance_meters", Prompt: "Допустимое расстояние от выбранных мест, метров?", Kind: QuestionInt},
		{ID: "add_6_may_park", Prompt: "Искать рядом с 6 May Park? да/нет", Kind: QuestionBool},
		{ID: "add_new_boulevard", Prompt: "Искать рядом с New Boulevard? да/нет", Kind: QuestionBool},
		{ID: "custom_locations", Prompt: "Другие районы, места или координаты? Несколько - через ;. Пусто или . чтобы пропустить", Kind: QuestionString},
		{ID: "desired_style", Prompt: "Какой ремонт нравится? Можно обычным текстом", Kind: QuestionString},
		{ID: "reject_glossy_ceilings", Prompt: "Отсеивать глянцевые потолки? да/нет", Kind: QuestionBool},
		{ID: "reject_dark_walls", Prompt: "Отсеивать темные стены? да/нет", Kind: QuestionBool},
		{ID: "reject_old_renovation", Prompt: "Отсеивать старый ремонт? да/нет", Kind: QuestionBool},
		{ID: "reject_windows_facing_house", Prompt: "Отсеивать окна прямо в соседний дом? да/нет", Kind: QuestionBool},
		{ID: "importance_location", Prompt: "Важность локации от 0 до 5?", Kind: QuestionImportance},
		{ID: "importance_interior", Prompt: "Важность интерьера от 0 до 5?", Kind: QuestionImportance},
		{ID: "importance_view", Prompt: "Важность вида из окна от 0 до 5?", Kind: QuestionImportance},
		{ID: "importance_price", Prompt: "Важность цены от 0 до 5?", Kind: QuestionImportance},
		{ID: "importance_appliances", Prompt: "Важность техники от 0 до 5?", Kind: QuestionImportance},
	}
}

func (s *Session) CurrentQuestion() (Question, bool) {
	questions := Questions()
	if s.done || s.step >= len(questions) {
		return Question{}, false
	}
	return questions[s.step], true
}

func (s *Session) Answer(input string) (Question, bool, error) {
	question, ok := s.CurrentQuestion()
	if !ok {
		return Question{}, true, nil
	}

	if err := s.applyAnswer(question, strings.TrimSpace(input)); err != nil {
		return question, false, err
	}

	s.step++
	if s.step >= len(Questions()) {
		s.done = true
		return Question{}, true, nil
	}

	next, _ := s.CurrentQuestion()
	return next, false, nil
}

func (s *Session) Preferences() domain.UserPreferences {
	return s.prefs
}

func (s *Session) Done() bool {
	return s.done
}

func (s *Session) applyAnswer(question Question, input string) error {
	switch question.Kind {
	case QuestionInt:
		value, err := strconv.Atoi(input)
		if err != nil {
			return fmt.Errorf("введите целое число")
		}
		s.setInt(question.ID, value)
	case QuestionFloat:
		value, err := strconv.ParseFloat(input, 64)
		if err != nil {
			return fmt.Errorf("введите число")
		}
		s.setFloat(question.ID, value)
	case QuestionBool:
		value, err := parseBoolInput(input)
		if err != nil {
			return err
		}
		if err := s.setBool(question.ID, value); err != nil {
			return err
		}
	case QuestionImportance:
		value, err := strconv.Atoi(input)
		if err != nil || value < 0 || value > 5 {
			return fmt.Errorf("введите число от 0 до 5")
		}
		s.setImportance(question.ID, value)
	case QuestionString:
		if err := s.setString(question.ID, input); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) setInt(id string, value int) {
	switch id {
	case "min_budget":
		s.prefs.MinBudget = value
	case "max_budget":
		s.prefs.MaxBudget = value
	case "max_distance_meters":
		s.distance = value
	}
}

func (s *Session) setFloat(id string, value float64) {
	if id == "min_area_m2" {
		s.prefs.MinAreaM2 = value
	}
}

func (s *Session) setBool(id string, value bool) error {
	switch id {
	case "require_dishwasher":
		s.prefs.RequireDishwasher = value
	case "require_oven":
		s.prefs.RequireOven = value
	case "add_6_may_park":
		if value {
			loc, err := s.resolveLocation("6 May Park")
			if err != nil {
				return err
			}
			s.prefs.Locations = append(s.prefs.Locations, loc)
		}
	case "add_new_boulevard":
		if value {
			loc, err := s.resolveLocation("New Boulevard")
			if err != nil {
				return err
			}
			s.prefs.Locations = append(s.prefs.Locations, loc)
		}
	case "reject_glossy_ceilings":
		s.prefs.Reject.GlossyCeilings = value
	case "reject_dark_walls":
		s.prefs.Reject.DarkWalls = value
	case "reject_old_renovation":
		s.prefs.Reject.OldRenovation = value
	case "reject_windows_facing_house":
		s.prefs.Reject.WindowsFacingHouse = value
	}
	return nil
}

func (s *Session) setString(id, value string) error {
	switch id {
	case "custom_locations":
		if value == "." {
			return nil
		}
		for _, name := range strings.Split(value, ";") {
			name = strings.TrimSpace(name)
			if name != "" && name != "." {
				loc, err := s.resolveLocation(name)
				if err != nil {
					return err
				}
				s.prefs.Locations = append(s.prefs.Locations, loc)
			}
		}
	case "desired_style":
		s.prefs.DesiredStyle = value
	}
	return nil
}

func (s *Session) setImportance(id string, value int) {
	switch id {
	case "importance_location":
		s.prefs.Importance.Location = value
	case "importance_interior":
		s.prefs.Importance.Interior = value
	case "importance_view":
		s.prefs.Importance.View = value
	case "importance_price":
		s.prefs.Importance.Price = value
	case "importance_appliances":
		s.prefs.Importance.Appliances = value
	}
}

func parseBoolInput(input string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "y", "yes", "да", "д", "+", "true", "1":
		return true, nil
	case "n", "no", "нет", "н", "-", "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("ответьте да или нет")
	}
}

func (s *Session) resolveLocation(name string) (domain.LocationPreference, error) {
	loc, err := s.resolver.Resolve(context.Background(), name, s.distance)
	if err != nil {
		return domain.LocationPreference{}, fmt.Errorf("не удалось найти локацию %q: %w", name, err)
	}
	if strings.TrimSpace(loc.Name) == "" {
		return domain.LocationPreference{}, fmt.Errorf("не удалось найти локацию %q", name)
	}
	if loc.Latitude == 0 || loc.Longitude == 0 {
		return loc, nil
	}
	return loc, nil
}
