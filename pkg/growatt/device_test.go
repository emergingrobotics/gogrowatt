package growatt

import (
	"context"
	"net/http"
	"testing"
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

	if devices[0].DeviceSN != "ABC123456" {
		t.Errorf("expected device SN %q, got %q", "ABC123456", devices[0].DeviceSN)
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

	if inverter.Pac != 4523.5 {
		t.Errorf("expected Pac %f, got %f", 4523.5, inverter.Pac)
	}

	if inverter.Etoday != 32.5 {
		t.Errorf("expected Etoday %f, got %f", 32.5, inverter.Etoday)
	}

	if inverter.Temperature != 42.5 {
		t.Errorf("expected temperature %f, got %f", 42.5, inverter.Temperature)
	}
}
