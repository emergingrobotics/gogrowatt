package growatt

import (
	"context"
	"net/url"
)

// ListDevices returns all devices for a plant
func (c *Client) ListDevices(ctx context.Context, plantID string) ([]Device, error) {
	params := url.Values{}
	params.Set("plant_id", plantID)

	body, err := c.get(ctx, "device/list", params)
	if err != nil {
		return nil, err
	}

	data, err := parseResponse[DeviceListData](body)
	if err != nil {
		return nil, err
	}

	return data.Devices, nil
}

// GetMINInverterDetails returns details for a MIN/TLX inverter
func (c *Client) GetMINInverterDetails(ctx context.Context, serial string) (*MINInverterData, error) {
	params := url.Values{}
	params.Set("tlx_sn", serial)

	body, err := c.get(ctx, "device/tlx/tlx_data_info", params)
	if err != nil {
		return nil, err
	}

	return parseResponse[MINInverterData](body)
}
