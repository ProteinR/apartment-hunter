package preferenceparser

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"apartment-hunter/internal/domain"
)

type PostGISGeometryBuilder struct {
	Resolver GeoResolver
	Segments int
}

func (b PostGISGeometryBuilder) BuildSearchGeometry(ctx context.Context, prefs []domain.LocationPreference) (*domain.GeoSearchArea, error) {
	segments := b.Segments
	if segments <= 0 {
		segments = 48
	}

	var polygons [][][]float64
	var unresolved []string
	for i := range prefs {
		loc := prefs[i]
		radius := radiusMeters(loc)
		if radius <= 0 {
			if loc.Type == "area" && strings.TrimSpace(loc.CenterName) != "" {
				unresolved = append(unresolved, "area requires polygon resolution: "+loc.CenterName)
			}
			continue
		}
		lat, lng, err := b.resolveCenter(ctx, loc)
		if err != nil {
			return nil, err
		}
		if lat == 0 && lng == 0 {
			unresolved = append(unresolved, "center unresolved: "+locationQuery(loc))
			continue
		}
		polygon := circlePolygon(lng, lat, radius, segments)
		newBoulevardSeaArea := isNewBoulevardLocation(loc) && hasSeaDistanceRule(loc)
		if newBoulevardSeaArea {
			polygon = batumiNewBoulevardSeaArea()
		}
		appliedRules := map[string]bool{}
		if clipped, ok, err := b.clipBoundaryStreet(ctx, polygon, loc, []float64{lng, lat}); err != nil {
			return nil, err
		} else if ok {
			polygon = clipped
			appliedRules["pushkin"] = true
		}
		if newBoulevardSeaArea {
			appliedRules["sea"] = true
		} else if clipped, ok := clipSeaDistanceRule(polygon, loc); ok {
			polygon = clipped
			appliedRules["sea"] = true
		}
		if len(polygon) >= 4 {
			polygons = append(polygons, polygon)
		} else {
			unresolved = append(unresolved, "geometry clipped to empty area: "+locationQuery(loc))
		}
		if loc.BoundaryStreetName != nil && strings.TrimSpace(*loc.BoundaryStreetName) != "" && !appliedRules["pushkin"] {
			unresolved = append(unresolved, "requires_manual_boundary_resolution: "+*loc.BoundaryStreetName)
		}
		unresolved = append(unresolved, unresolvedRulesAfterApplied(loc.UnresolvedRules, appliedRules)...)
	}

	area := &domain.GeoSearchArea{
		SRID:            4326,
		UnresolvedRules: unresolved,
	}
	if len(polygons) == 0 {
		return area, nil
	}
	area.WKT = multiPolygonWKT(polygons)
	geoJSON, err := multiPolygonGeoJSON(polygons)
	if err != nil {
		return nil, err
	}
	area.GeoJSON = geoJSON
	return area, nil
}

func (b PostGISGeometryBuilder) clipBoundaryStreet(ctx context.Context, polygon [][]float64, loc domain.LocationPreference, keepPoint []float64) ([][]float64, bool, error) {
	streetName, ok := boundaryStreetName(loc)
	if !ok {
		return polygon, false, nil
	}
	line, ok, err := b.resolveBoundaryLine(ctx, streetName)
	if err != nil || !ok {
		return polygon, false, err
	}
	clipped := clipPolygonByLine(polygon, line[0], line[1], keepPoint)
	return clipped, true, nil
}

func (b PostGISGeometryBuilder) resolveBoundaryLine(ctx context.Context, streetName string) ([]geomPoint, bool, error) {
	if b.Resolver != nil {
		street, err := b.Resolver.ResolveStreet(ctx, streetName)
		if err != nil {
			return nil, false, err
		}
		if street != nil {
			if line, ok := parseLineStringWKT(street.GeometryWKT); ok {
				return line, true, nil
			}
		}
	}
	if isPushkinStreet(streetName) {
		return knownPushkinBoundaryLine(), true, nil
	}
	return nil, false, nil
}

func (b PostGISGeometryBuilder) resolveCenter(ctx context.Context, loc domain.LocationPreference) (float64, float64, error) {
	if loc.Latitude != 0 || loc.Longitude != 0 {
		return loc.Latitude, loc.Longitude, nil
	}
	if lat, lng, ok := knownBatumiCenter(loc); ok {
		return lat, lng, nil
	}
	if b.Resolver == nil {
		return 0, 0, nil
	}
	place, err := b.Resolver.ResolvePlace(ctx, locationQuery(loc))
	if err != nil || place == nil {
		return 0, 0, err
	}
	return place.Latitude, place.Longitude, nil
}

func radiusMeters(loc domain.LocationPreference) int {
	if loc.RadiusMeters != nil {
		return *loc.RadiusMeters
	}
	return loc.MaxDistanceMeters
}

func locationQuery(loc domain.LocationPreference) string {
	if strings.TrimSpace(loc.CenterName) != "" {
		return strings.TrimSpace(loc.CenterName)
	}
	return strings.TrimSpace(loc.Name)
}

func knownBatumiCenter(loc domain.LocationPreference) (float64, float64, bool) {
	if isNewBoulevardLocation(loc) {
		return 41.6172, 41.5945, true
	}
	text := locationPreferenceText(loc)
	if strings.Contains(text, "6 may park") || strings.Contains(text, "six may park") || strings.Contains(text, "парк 6 мая") {
		return 41.64800157, 41.6285588, true
	}
	return 0, 0, false
}

func isNewBoulevardLocation(loc domain.LocationPreference) bool {
	text := locationPreferenceText(loc)
	return strings.Contains(text, "new boulevard") || strings.Contains(text, "новый бульвар")
}

func locationPreferenceText(loc domain.LocationPreference) string {
	return strings.ToLower(strings.TrimSpace(strings.Join([]string{
		loc.Name,
		loc.CenterName,
		loc.RawText,
	}, " ")))
}

func circlePolygon(lng, lat float64, radiusMeters, segments int) [][]float64 {
	const earthRadiusMeters = 6378137.0
	coords := make([][]float64, 0, segments+1)
	latRad := lat * math.Pi / 180
	lngRad := lng * math.Pi / 180
	angularDistance := float64(radiusMeters) / earthRadiusMeters
	for i := 0; i <= segments; i++ {
		bearing := 2 * math.Pi * float64(i) / float64(segments)
		pointLat := math.Asin(math.Sin(latRad)*math.Cos(angularDistance) + math.Cos(latRad)*math.Sin(angularDistance)*math.Cos(bearing))
		pointLng := lngRad + math.Atan2(math.Sin(bearing)*math.Sin(angularDistance)*math.Cos(latRad), math.Cos(angularDistance)-math.Sin(latRad)*math.Sin(pointLat))
		coords = append(coords, []float64{pointLng * 180 / math.Pi, pointLat * 180 / math.Pi})
	}
	return coords
}

func multiPolygonWKT(polygons [][][]float64) string {
	var b strings.Builder
	b.WriteString("MULTIPOLYGON(")
	for i, polygon := range polygons {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("((")
		for j, coord := range polygon {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%.8f %.8f", coord[0], coord[1])
		}
		b.WriteString("))")
	}
	b.WriteString(")")
	return b.String()
}

func multiPolygonGeoJSON(polygons [][][]float64) (string, error) {
	coordinates := make([][][][]float64, 0, len(polygons))
	for _, polygon := range polygons {
		coordinates = append(coordinates, [][][]float64{polygon})
	}
	data, err := json.Marshal(map[string]any{
		"type":        "MultiPolygon",
		"coordinates": coordinates,
	})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type geomPoint struct {
	Lng float64
	Lat float64
}

func boundaryStreetName(loc domain.LocationPreference) (string, bool) {
	if loc.BoundaryStreetName != nil && strings.TrimSpace(*loc.BoundaryStreetName) != "" {
		return strings.TrimSpace(*loc.BoundaryStreetName), true
	}
	for _, text := range append([]string{loc.RawText, stringValue(loc.BoundaryRule)}, loc.UnresolvedRules...) {
		if isPushkinStreet(text) {
			return "Pushkin Street, Batumi", true
		}
	}
	return "", false
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func isPushkinStreet(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(text, "pushkin") || strings.Contains(text, "пушкин")
}

func knownPushkinBoundaryLine() []geomPoint {
	return []geomPoint{
		{Lng: 41.6200, Lat: 41.6370},
		{Lng: 41.6530, Lat: 41.6465},
	}
}

func unresolvedRulesAfterApplied(rules []string, applied map[string]bool) []string {
	var unresolved []string
	for _, rule := range rules {
		if applied["pushkin"] && isPushkinStreet(rule) {
			continue
		}
		if applied["sea"] && mentionsSea(rule) {
			continue
		}
		unresolved = append(unresolved, rule)
	}
	return unresolved
}

func clipSeaDistanceRule(polygon [][]float64, loc domain.LocationPreference) ([][]float64, bool) {
	if !hasSeaDistanceRule(loc) {
		return polygon, false
	}
	clipped := clipPolygonByConvexPolygon(polygon, batumiOneKilometerSeaCorridor())
	return clipped, true
}

func hasSeaDistanceRule(loc domain.LocationPreference) bool {
	return mentionsSea(loc.RawText) || mentionsSea(strings.Join(loc.UnresolvedRules, " "))
}

func mentionsSea(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(text, "море") ||
		strings.Contains(text, "моря") ||
		strings.Contains(text, "sea") ||
		strings.Contains(text, "coast")
}

func batumiOneKilometerSeaCorridor() [][]float64 {
	return closePolygon([][]float64{
		{41.5760, 41.6060},
		{41.5940, 41.6160},
		{41.6140, 41.6300},
		{41.6370, 41.6480},
		{41.6530, 41.6590},
		{41.6630, 41.6500},
		{41.6440, 41.6370},
		{41.6220, 41.6200},
		{41.6020, 41.6060},
		{41.5860, 41.5980},
	})
}

func batumiNewBoulevardSeaArea() [][]float64 {
	return closePolygon([][]float64{
		{41.5735, 41.5980},
		{41.5875, 41.6105},
		{41.6035, 41.6245},
		{41.6185, 41.6340},
		{41.6280, 41.6265},
		{41.6115, 41.6125},
		{41.5950, 41.5985},
		{41.5815, 41.5895},
	})
}

func parseLineStringWKT(wkt string) ([]geomPoint, bool) {
	wkt = strings.TrimSpace(wkt)
	lower := strings.ToLower(wkt)
	if !strings.HasPrefix(lower, "linestring(") || !strings.HasSuffix(wkt, ")") {
		return nil, false
	}
	body := strings.TrimSpace(wkt[len("LINESTRING(") : len(wkt)-1])
	parts := strings.Split(body, ",")
	line := make([]geomPoint, 0, len(parts))
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) < 2 {
			return nil, false
		}
		lng, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, false
		}
		lat, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, false
		}
		line = append(line, geomPoint{Lng: lng, Lat: lat})
	}
	if len(line) < 2 {
		return nil, false
	}
	return []geomPoint{line[0], line[len(line)-1]}, true
}

func clipPolygonByLine(polygon [][]float64, a, b geomPoint, keepPoint []float64) [][]float64 {
	if len(polygon) < 4 || len(keepPoint) < 2 {
		return polygon
	}
	open := openPolygon(polygon)
	keepSide := lineSide(a, b, geomPoint{Lng: keepPoint[0], Lat: keepPoint[1]})
	output := make([][]float64, 0, len(open))
	for i, current := range open {
		previous := open[(i+len(open)-1)%len(open)]
		currentInside := sameLineSide(a, b, current, keepSide)
		previousInside := sameLineSide(a, b, previous, keepSide)
		if currentInside {
			if !previousInside {
				output = append(output, lineIntersection(previous, current, a, b))
			}
			output = append(output, current)
		} else if previousInside {
			output = append(output, lineIntersection(previous, current, a, b))
		}
	}
	return closePolygon(output)
}

func clipPolygonByConvexPolygon(subject, clipper [][]float64) [][]float64 {
	output := openPolygon(subject)
	clip := openPolygon(clipper)
	if signedArea(clip) < 0 {
		reversePolygon(clip)
	}
	for i, aCoord := range clip {
		if len(output) == 0 {
			return nil
		}
		bCoord := clip[(i+1)%len(clip)]
		a := geomPoint{Lng: aCoord[0], Lat: aCoord[1]}
		b := geomPoint{Lng: bCoord[0], Lat: bCoord[1]}
		input := output
		output = nil
		for j, current := range input {
			previous := input[(j+len(input)-1)%len(input)]
			currentInside := lineSide(a, b, geomPoint{Lng: current[0], Lat: current[1]}) >= -1e-12
			previousInside := lineSide(a, b, geomPoint{Lng: previous[0], Lat: previous[1]}) >= -1e-12
			if currentInside {
				if !previousInside {
					output = append(output, lineIntersection(previous, current, a, b))
				}
				output = append(output, current)
			} else if previousInside {
				output = append(output, lineIntersection(previous, current, a, b))
			}
		}
	}
	return closePolygon(output)
}

func sameLineSide(a, b geomPoint, point []float64, keepSide float64) bool {
	side := lineSide(a, b, geomPoint{Lng: point[0], Lat: point[1]})
	if math.Abs(keepSide) < 1e-12 {
		return side >= -1e-12
	}
	return side*keepSide >= -1e-12
}

func lineSide(a, b, p geomPoint) float64 {
	return (b.Lng-a.Lng)*(p.Lat-a.Lat) - (b.Lat-a.Lat)*(p.Lng-a.Lng)
}

func lineIntersection(p1, p2 []float64, a, b geomPoint) []float64 {
	s1 := lineSide(a, b, geomPoint{Lng: p1[0], Lat: p1[1]})
	s2 := lineSide(a, b, geomPoint{Lng: p2[0], Lat: p2[1]})
	denominator := s1 - s2
	if math.Abs(denominator) < 1e-12 {
		return []float64{p2[0], p2[1]}
	}
	t := s1 / denominator
	return []float64{
		p1[0] + t*(p2[0]-p1[0]),
		p1[1] + t*(p2[1]-p1[1]),
	}
}

func openPolygon(polygon [][]float64) [][]float64 {
	if len(polygon) > 1 {
		first := polygon[0]
		last := polygon[len(polygon)-1]
		if len(first) >= 2 && len(last) >= 2 && math.Abs(first[0]-last[0]) < 1e-12 && math.Abs(first[1]-last[1]) < 1e-12 {
			return append([][]float64(nil), polygon[:len(polygon)-1]...)
		}
	}
	return append([][]float64(nil), polygon...)
}

func closePolygon(polygon [][]float64) [][]float64 {
	if len(polygon) == 0 {
		return nil
	}
	closed := append([][]float64(nil), polygon...)
	first := closed[0]
	last := closed[len(closed)-1]
	if len(first) < 2 || len(last) < 2 || math.Abs(first[0]-last[0]) >= 1e-12 || math.Abs(first[1]-last[1]) >= 1e-12 {
		closed = append(closed, []float64{first[0], first[1]})
	}
	if len(closed) < 4 {
		return nil
	}
	return closed
}

func signedArea(polygon [][]float64) float64 {
	area := 0.0
	for i, current := range polygon {
		next := polygon[(i+1)%len(polygon)]
		area += current[0]*next[1] - next[0]*current[1]
	}
	return area / 2
}

func reversePolygon(polygon [][]float64) {
	for i, j := 0, len(polygon)-1; i < j; i, j = i+1, j-1 {
		polygon[i], polygon[j] = polygon[j], polygon[i]
	}
}
