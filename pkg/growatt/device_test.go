package growatt

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestListDevices(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/device/list" {
			t.Errorf("expected path /device/list, got %s", r.URL.Path)
		}

		plantID := r.URL.Query().Get("plant_id")
		if plantID != "12345" {
			t.Errorf("expected plant_id %q, got %q", "12345", plantID)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadTestData(t, "device_list.json"))
	})
	defer server.Close()

	client := newTestClient(t, server)
	ctx := context.Background()

	devices, err := client.ListDevices(ctx, "12345")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(devices) != 1 {
		t.Errorf("expected 1 device, got %d", len(devices))
	}

	if devices[0].DeviceSN.String() != "ABC123456" {
		t.Errorf("expected device SN %q, got %q", "ABC123456", devices[0].DeviceSN.String())
	}

	if devices[0].DeviceType != 7 {
		t.Errorf("expected device type %d, got %d", 7, devices[0].DeviceType)
	}

	if devices[0].Model != "MIN 9000TL-X" {
		t.Errorf("expected model %q, got %q", "MIN 9000TL-X", devices[0].Model)
	}
}

func TestGetMINInverterDetails(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/device/tlx/tlx_data_info" {
			t.Errorf("expected path /device/tlx/tlx_data_info, got %s", r.URL.Path)
		}

		serial := r.URL.Query().Get("tlx_sn")
		if serial != "ABC123456" {
			t.Errorf("expected tlx_sn %q, got %q", "ABC123456", serial)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadTestData(t, "min_inverter.json"))
	})
	defer server.Close()

	client := newTestClient(t, server)
	ctx := context.Background()

	inverter, err := client.GetMINInverterDetails(ctx, "ABC123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if inverter.Serial != "ABC123456" {
		t.Errorf("expected serial %q, got %q", "ABC123456", inverter.Serial)
	}

	if inverter.Pac.Float64() != 4523.5 {
		t.Errorf("expected Pac %f, got %f", 4523.5, inverter.Pac.Float64())
	}

	if inverter.Etoday.Float64() != 32.5 {
		t.Errorf("expected Etoday %f, got %f", 32.5, inverter.Etoday.Float64())
	}

	if inverter.Temperature.Float64() != 42.5 {
		t.Errorf("expected temperature %f, got %f", 42.5, inverter.Temperature.Float64())
	}
}

func TestGetMINInverterHistoryDetail(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/device/tlx/tlx_data" {
			t.Errorf("expected path /device/tlx/tlx_data, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		r.ParseForm()
		if sn := r.FormValue("tlx_sn"); sn != "ABC123456" {
			t.Errorf("expected tlx_sn %q, got %q", "ABC123456", sn)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadTestData(t, "min_history.json"))
	})
	defer server.Close()

	client := newTestClient(t, server)
	ctx := context.Background()

	date := time.Date(2025, 2, 15, 0, 0, 0, 0, time.UTC)
	points, err := client.GetMINInverterHistoryDetail(ctx, "ABC123456", date, "US/Central")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(points) != 3 {
		t.Fatalf("expected 3 data points, got %d", len(points))
	}

	// Verify sorted by time (fixture is intentionally out of order)
	if points[0].Time > points[1].Time || points[1].Time > points[2].Time {
		t.Errorf("data points not sorted by time: %q, %q, %q", points[0].Time, points[1].Time, points[2].Time)
	}

	// Verify the latest data point (last after sort) has all fields
	latest := points[2]
	assertFlexFloat(t, "Pac", latest.Pac, 3500.0)
	assertFlexFloat(t, "Ppv", latest.Ppv, 3700.0)
	assertFlexFloat(t, "Vpv1", latest.Vpv1, 382.0)
	assertFlexFloat(t, "Vpv2", latest.Vpv2, 377.0)
	assertFlexFloat(t, "Ipv1", latest.Ipv1, 5.2)
	assertFlexFloat(t, "Ipv2", latest.Ipv2, 5.0)
	assertFlexFloat(t, "Vac1", latest.Vac1, 240.8)
	assertFlexFloat(t, "Iac1", latest.Iac1, 14.5)
}

func assertFlexFloat(t *testing.T, name string, got FlexFloat, want float64) {
	t.Helper()
	if math.Abs(got.Float64()-want) > 0.01 {
		t.Errorf("%s = %f, want %f", name, got.Float64(), want)
	}
}
