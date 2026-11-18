package certmanager

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
)

// ReplaceARecord requires a single dedicated A record at either the expected
// old or new address. PATCH changes content only, preserving DNS attributes.
func (c *CloudflareDNS) ReplaceARecord(ctx context.Context, fqdn string, oldIP, newIP netip.Addr, apply bool) error {
	name, err := canonicalSNI(fqdn)
	if err != nil || !oldIP.Is4() || !newIP.Is4() {
		return ErrInvalidInput
	}
	zone, records, err := c.lookupARecords(ctx, name)
	if err != nil {
		return err
	}
	if len(records) != 1 || records[0].ID == "" || records[0].Type != "A" || records[0].Name != name {
		return safe("Expected one dedicated Cloudflare A record", ErrDistributionFailed)
	}
	record := records[0]
	if record.Content == newIP.String() {
		return nil
	}
	if record.Content != oldIP.String() {
		return safe("Cloudflare address changed since preview", ErrDistributionFailed)
	}
	if !apply {
		return nil
	}
	var result cloudflareResponse[cloudflareRecord]
	if err := c.call(ctx, http.MethodPatch, "zones/"+url.PathEscape(zone)+"/dns_records/"+url.PathEscape(record.ID), map[string]any{"content": newIP.String()}, &result); err != nil {
		return err
	}
	if !result.Success || result.Result.Content != newIP.String() {
		return safe("Cloudflare address update was not confirmed", ErrDistributionFailed)
	}
	return nil
}
