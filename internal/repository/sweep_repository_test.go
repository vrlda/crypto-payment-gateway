package repository

import (
	"testing"

	"crypto_payment_gateway_core/internal/model"
)

func TestActiveSweepStatusesIncludeEnergyRentalWait(t *testing.T) {
	t.Parallel()

	statuses := activeSweepStatuses()
	if !containsSweepStatus(statuses, model.SweepStatusWaitingForEnergyRental) {
		t.Fatalf("activeSweepStatuses() = %v, want %q included", statuses, model.SweepStatusWaitingForEnergyRental)
	}
}

func TestCoveredSweepStatusesIncludeEnergyRentalWait(t *testing.T) {
	t.Parallel()

	statuses := coveredSweepStatuses()
	if !containsSweepStatus(statuses, model.SweepStatusWaitingForEnergyRental) {
		t.Fatalf("coveredSweepStatuses() = %v, want %q included", statuses, model.SweepStatusWaitingForEnergyRental)
	}
}

func TestBlockingSweepStatusesIncludeEnergyRentalWait(t *testing.T) {
	t.Parallel()

	statuses := blockingSweepStatuses()
	if !containsSweepStatus(statuses, model.SweepStatusWaitingForEnergyRental) {
		t.Fatalf("blockingSweepStatuses() = %v, want %q included", statuses, model.SweepStatusWaitingForEnergyRental)
	}
}

func containsSweepStatus(statuses []model.SweepStatus, target model.SweepStatus) bool {
	for _, status := range statuses {
		if status == target {
			return true
		}
	}
	return false
}
