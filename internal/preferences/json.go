package preferences

import (
	"encoding/json"
	"os"

	"apartment-hunter/internal/domain"
)

func SaveJSON(path string, prefs domain.UserPreferences) error {
	data, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}
