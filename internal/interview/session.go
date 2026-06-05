package interview

import (
	"fmt"
	"strconv"
	"strings"

	"apartment-hunter/internal/adapters/geocodingmock"
	"apartment-hunter/internal/domain"
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
}

func NewSession() *Session {
	return &Session{}
}

func Questions() []Question {
	return []Question{
		{ID: "min_budget", Prompt: "Minimum monthly budget (0 if none)", Kind: QuestionInt},
		{ID: "max_budget", Prompt: "Maximum monthly budget", Kind: QuestionInt},
		{ID: "min_area_m2", Prompt: "Minimum area, m2", Kind: QuestionFloat},
		{ID: "require_dishwasher", Prompt: "Dishwasher required (y/n)", Kind: QuestionBool},
		{ID: "require_oven", Prompt: "Oven required (y/n)", Kind: QuestionBool},
		{ID: "max_distance_meters", Prompt: "Allowed distance from selected districts, meters", Kind: QuestionInt},
		{ID: "add_6_may_park", Prompt: "Add 6 May Park (y/n)", Kind: QuestionBool},
		{ID: "add_new_boulevard", Prompt: "Add New Boulevard (y/n)", Kind: QuestionBool},
		{ID: "custom_locations", Prompt: "Custom districts or places, comma-separated (empty to skip)", Kind: QuestionString},
		{ID: "desired_style", Prompt: "Desired renovation style", Kind: QuestionString},
		{ID: "reject_glossy_ceilings", Prompt: "Reject glossy ceilings (y/n)", Kind: QuestionBool},
		{ID: "reject_dark_walls", Prompt: "Reject dark walls (y/n)", Kind: QuestionBool},
		{ID: "reject_old_renovation", Prompt: "Reject old renovation (y/n)", Kind: QuestionBool},
		{ID: "reject_windows_facing_house", Prompt: "Reject windows facing neighboring house (y/n)", Kind: QuestionBool},
		{ID: "importance_location", Prompt: "Importance of location from 0 to 5", Kind: QuestionImportance},
		{ID: "importance_interior", Prompt: "Importance of interior from 0 to 5", Kind: QuestionImportance},
		{ID: "importance_view", Prompt: "Importance of view from 0 to 5", Kind: QuestionImportance},
		{ID: "importance_price", Prompt: "Importance of price from 0 to 5", Kind: QuestionImportance},
		{ID: "importance_appliances", Prompt: "Importance of appliances from 0 to 5", Kind: QuestionImportance},
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
			return fmt.Errorf("enter an integer")
		}
		s.setInt(question.ID, value)
	case QuestionFloat:
		value, err := strconv.ParseFloat(input, 64)
		if err != nil {
			return fmt.Errorf("enter a number")
		}
		s.setFloat(question.ID, value)
	case QuestionBool:
		value, err := parseBoolInput(input)
		if err != nil {
			return err
		}
		s.setBool(question.ID, value)
	case QuestionImportance:
		value, err := strconv.Atoi(input)
		if err != nil || value < 0 || value > 5 {
			return fmt.Errorf("use a value from 0 to 5")
		}
		s.setImportance(question.ID, value)
	case QuestionString:
		s.setString(question.ID, input)
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

func (s *Session) setBool(id string, value bool) {
	switch id {
	case "require_dishwasher":
		s.prefs.RequireDishwasher = value
	case "require_oven":
		s.prefs.RequireOven = value
	case "add_6_may_park":
		if value {
			s.prefs.Locations = append(s.prefs.Locations, geocodingmock.Resolve("6 May Park", s.distance))
		}
	case "add_new_boulevard":
		if value {
			s.prefs.Locations = append(s.prefs.Locations, geocodingmock.Resolve("New Boulevard", s.distance))
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
}

func (s *Session) setString(id, value string) {
	switch id {
	case "custom_locations":
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				s.prefs.Locations = append(s.prefs.Locations, geocodingmock.Resolve(name, s.distance))
			}
		}
	case "desired_style":
		s.prefs.DesiredStyle = value
	}
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
	case "y", "yes", "да", "true", "1":
		return true, nil
	case "n", "no", "нет", "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("enter y or n")
	}
}
