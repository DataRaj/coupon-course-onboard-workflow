package webhook

import "strings"

// providerRefs are the identifiers a webhook payload may expose. Pabbly nests them
// differently per event, so we search the document rather than assume one shape.
type providerRefs struct {
	customerID    string
	invoiceID     string
	transactionID string
	productID     string
	planID        string
	couponCode    string
}

var refKeys = map[string][]string{
	"customer":    {"customer_id", "customerid", "customer"},
	"invoice":     {"invoice_id", "invoiceid", "invoice"},
	"transaction": {"payment_id", "transaction_id", "paymentid", "transactionid"},
	"product":     {"product_id", "productid"},
	"plan":        {"plan_id", "planid"},
	"coupon":      {"coupon_code", "couponcode", "coupon"},
}

func extractRefs(payload map[string]any) providerRefs {
	found := map[string]string{}
	walk(payload, found, 0)
	return providerRefs{
		customerID:    found["customer"],
		invoiceID:     found["invoice"],
		transactionID: found["transaction"],
		productID:     found["product"],
		planID:        found["plan"],
		couponCode:    found["coupon"],
	}
}

// walk records the first value seen for each identifier, breadth of nesting aside.
func walk(node any, found map[string]string, depth int) {
	if depth > 6 {
		return
	}
	switch t := node.(type) {
	case map[string]any:
		for key, value := range t {
			lower := strings.ToLower(key)
			for field, candidates := range refKeys {
				if found[field] != "" {
					continue
				}
				for _, candidate := range candidates {
					if lower == candidate {
						if s := stringify(value); s != "" {
							found[field] = s
						}
					}
				}
			}
			walk(value, found, depth+1)
		}
	case []any:
		for _, item := range t {
			walk(item, found, depth+1)
		}
	}
}

func stringify(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}
