package preferences

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"apartment-hunter/internal/domain"
)

func Save(path string, prefs domain.UserPreferences) error {
	return os.WriteFile(path, []byte(Render(prefs)), 0644)
}

func Load(path string) (domain.UserPreferences, error) {
	file, err := os.Open(path)
	if err != nil {
		return domain.UserPreferences{}, err
	}
	defer file.Close()

	var prefs domain.UserPreferences
	section := ""
	locationIndex := -1
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, ":") && !strings.HasPrefix(line, "-") {
			section = strings.TrimSuffix(line, ":")
			continue
		}
		if strings.HasPrefix(line, "- name:") {
			locationIndex++
			prefs.Locations = append(prefs.Locations, domain.LocationPreference{Name: cleanValue(afterColon(line))})
			section = "locations"
			continue
		}

		key, value, ok := splitKV(line)
		if !ok {
			continue
		}
		switch section {
		case "locations":
			if locationIndex >= 0 {
				setLocationField(&prefs.Locations[locationIndex], key, value)
			}
		case "reject":
			setRejectField(&prefs.Reject, key, value)
		case "importance":
			setImportanceField(&prefs.Importance, key, value)
		default:
			setTopField(&prefs, key, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return domain.UserPreferences{}, err
	}
	return prefs, nil
}

func Render(prefs domain.UserPreferences) string {
	var b strings.Builder
	fmt.Fprintf(&b, "min_budget: %d\n", prefs.MinBudget)
	fmt.Fprintf(&b, "max_budget: %d\n", prefs.MaxBudget)
	fmt.Fprintf(&b, "min_area_m2: %.1f\n", prefs.MinAreaM2)
	fmt.Fprintf(&b, "require_dishwasher: %t\n", prefs.RequireDishwasher)
	fmt.Fprintf(&b, "require_oven: %t\n", prefs.RequireOven)
	fmt.Fprintf(&b, "desired_style: %s\n", quoteIfNeeded(prefs.DesiredStyle))
	b.WriteString("locations:\n")
	for _, loc := range prefs.Locations {
		fmt.Fprintf(&b, "  - name: %s\n", quoteIfNeeded(loc.Name))
		fmt.Fprintf(&b, "    max_distance_meters: %d\n", loc.MaxDistanceMeters)
		if loc.Latitude != 0 || loc.Longitude != 0 {
			fmt.Fprintf(&b, "    latitude: %.6f\n", loc.Latitude)
			fmt.Fprintf(&b, "    longitude: %.6f\n", loc.Longitude)
		}
	}
	b.WriteString("reject:\n")
	fmt.Fprintf(&b, "  glossy_ceilings: %t\n", prefs.Reject.GlossyCeilings)
	fmt.Fprintf(&b, "  dark_walls: %t\n", prefs.Reject.DarkWalls)
	fmt.Fprintf(&b, "  old_renovation: %t\n", prefs.Reject.OldRenovation)
	fmt.Fprintf(&b, "  windows_facing_house: %t\n", prefs.Reject.WindowsFacingHouse)
	b.WriteString("importance:\n")
	fmt.Fprintf(&b, "  location: %d\n", prefs.Importance.Location)
	fmt.Fprintf(&b, "  interior: %d\n", prefs.Importance.Interior)
	fmt.Fprintf(&b, "  view: %d\n", prefs.Importance.View)
	fmt.Fprintf(&b, "  price: %d\n", prefs.Importance.Price)
	fmt.Fprintf(&b, "  appliances: %d\n", prefs.Importance.Appliances)
	return b.String()
}

func setTopField(prefs *domain.UserPreferences, key, value string) {
	switch key {
	case "min_budget":
		prefs.MinBudget = parseInt(value)
	case "max_budget":
		prefs.MaxBudget = parseInt(value)
	case "min_area_m2":
		prefs.MinAreaM2 = parseFloat(value)
	case "require_dishwasher":
		prefs.RequireDishwasher = parseBool(value)
	case "require_oven":
		prefs.RequireOven = parseBool(value)
	case "desired_style":
		prefs.DesiredStyle = cleanValue(value)
	}
}

func setLocationField(loc *domain.LocationPreference, key, value string) {
	switch key {
	case "max_distance_meters":
		loc.MaxDistanceMeters = parseInt(value)
	case "latitude":
		loc.Latitude = parseFloat(value)
	case "longitude":
		loc.Longitude = parseFloat(value)
	}
}

func setRejectField(reject *domain.RejectionPreferences, key, value string) {
	switch key {
	case "glossy_ceilings":
		reject.GlossyCeilings = parseBool(value)
	case "dark_walls":
		reject.DarkWalls = parseBool(value)
	case "old_renovation":
		reject.OldRenovation = parseBool(value)
	case "windows_facing_house":
		reject.WindowsFacingHouse = parseBool(value)
	}
}

func setImportanceField(weights *domain.ImportanceWeights, key, value string) {
	switch key {
	case "location":
		weights.Location = parseInt(value)
	case "interior":
		weights.Interior = parseInt(value)
	case "view":
		weights.View = parseInt(value)
	case "price":
		weights.Price = parseInt(value)
	case "appliances":
		weights.Appliances = parseInt(value)
	}
}

func splitKV(line string) (string, string, bool) {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}

func afterColon(line string) string {
	_, value, _ := splitKV(line)
	return value
}

func cleanValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"`)
	return value
}

func quoteIfNeeded(value string) string {
	if value == "" {
		return `""`
	}
	if strings.ContainsAny(value, ":#") {
		return strconv.Quote(value)
	}
	return value
}

func parseInt(value string) int {
	out, _ := strconv.Atoi(cleanValue(value))
	return out
}

func parseFloat(value string) float64 {
	out, _ := strconv.ParseFloat(cleanValue(value), 64)
	return out
}

func parseBool(value string) bool {
	out, _ := strconv.ParseBool(cleanValue(value))
	return out
}
