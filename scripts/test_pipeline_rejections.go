package main

import (
	"fmt"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/evaluation"
)

func main() {
	// 1. Test AI Filter Rejection (e.g. required appliances missing, dark walls, etc.)
	{
		prefs := domain.UserPreferences{
			MaxBudget:         600,
			MinAreaM2:         50,
			RequireDishwasher: true,
			RequireOven:       true,
		}
		record := domain.ListingEvaluationRecord{
			Listing: domain.ApartmentListing{
				Title: "Квартира с темными стенами",
				Price: 500,
				AreaM2: 60,
			},
			Status: "rejected_ai",
			Score: &domain.ListingScore{
				PassedHardFilters: false,
				RejectionReasons: []string{
					"dishwasher is required",
					"dark walls detected",
				},
			},
		}
		report := evaluation.BuildReport(record, prefs)
		fmt.Println("=== CASE 1: AI Filter Rejection ===")
		fmt.Println(report)
		fmt.Println()
	}

	// 2. Test Basic Filter Rejection (e.g. price limits)
	{
		prefs := domain.UserPreferences{
			MaxBudget:         600,
			MinAreaM2:         50,
		}
		record := domain.ListingEvaluationRecord{
			Listing: domain.ApartmentListing{
				Title: "Слишком дорогая квартира",
				Price: 850,
				AreaM2: 60,
			},
			Status: "rejected_basic",
			Score: &domain.ListingScore{
				PassedHardFilters: false,
				RejectionReasons: []string{
					"price 850 exceeds max budget 600",
				},
			},
		}
		report := evaluation.BuildReport(record, prefs)
		fmt.Println("=== CASE 2: Basic Filter Rejection ===")
		fmt.Println(report)
		fmt.Println()
	}

	// 3. Test Location Rejection
	{
		prefs := domain.UserPreferences{
			Locations: []domain.LocationPreference{{Name: "Batumi"}},
		}
		record := domain.ListingEvaluationRecord{
			Listing: domain.ApartmentListing{
				Title: "Квартира в Кобулети",
				Price: 500,
				AreaM2: 55,
			},
			Status: "rejected_location",
			Score: &domain.ListingScore{
				PassedHardFilters: false,
				RejectionReasons: []string{
					"listing location is outside preferred search area",
				},
			},
		}
		report := evaluation.BuildReport(record, prefs)
		fmt.Println("=== CASE 3: Location Rejection ===")
		fmt.Println(report)
		fmt.Println()
	}
}
