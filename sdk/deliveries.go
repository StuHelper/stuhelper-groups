package sdk

import (
	"context"
	"net/http"
)

// ListDeliveriesParams and ListDeliveriesResponse share the published 0.4 types.
type ListDeliveriesParams = ListDeliverysParams
type ListDeliveriesResponse = ListDeliverysResponse

func NewListDeliveriesRequest(server, group string, params *ListDeliveriesParams) (*http.Request, error) {
	return NewListDeliverysRequest(server, group, params)
}
func ParseListDeliveriesResponse(response *http.Response) (*ListDeliveriesResponse, error) {
	return ParseListDeliverysResponse(response)
}
func (c *Client) ListDeliveries(ctx context.Context, group string, params *ListDeliveriesParams, editors ...RequestEditorFn) (*http.Response, error) {
	return c.ListDeliverys(ctx, group, params, editors...)
}
func (c *ClientWithResponses) ListDeliveries(ctx context.Context, group string, params *ListDeliveriesParams, editors ...RequestEditorFn) (*http.Response, error) {
	return c.ClientInterface.ListDeliverys(ctx, group, params, editors...)
}
func (c *ClientWithResponses) ListDeliveriesWithResponse(ctx context.Context, group string, params *ListDeliveriesParams, editors ...RequestEditorFn) (*ListDeliveriesResponse, error) {
	return c.ListDeliverysWithResponse(ctx, group, params, editors...)
}
