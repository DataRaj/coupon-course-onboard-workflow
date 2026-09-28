package pabbly

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"course-coupon/internal/money"
	"course-coupon/internal/provider"
)

// VerifyPurchase re-reads the invoice (and the customer's transactions when useful)
// straight from Pabbly. A webhook alone is never enough to confirm payment.
func (c *Client) VerifyPurchase(ctx context.Context, ref provider.PurchaseRef) (provider.PurchaseState, error) {
	state := provider.PurchaseState{
		InvoiceID:     ref.InvoiceID,
		CustomerID:    ref.CustomerID,
		TransactionID: ref.TransactionID,
	}

	if ref.InvoiceID != "" {
		var raw json.RawMessage
		if err := c.get(ctx, "/invoice/"+url.PathEscape(ref.InvoiceID), nil, &raw); err != nil {
			return state, err
		}
		inv := single(raw, "invoice", "doc", "result")
		if inv == nil {
			return state, fmt.Errorf("%w: invoice %s", provider.ErrNotFound, ref.InvoiceID)
		}

		status := strings.ToLower(inv.str("status", "invoice_status", "payment_status"))
		state.Paid = status == "paid" || status == "success" || status == "successful"
		state.Failed = status == "failed" || status == "failure"
		state.ProductID = firstNonEmpty(inv.str("product_id"), state.ProductID)
		state.PlanID = firstNonEmpty(inv.str("plan_id"), state.PlanID)
		state.CustomerID = firstNonEmpty(inv.str("customer_id"), state.CustomerID)
		state.Currency = currencyOr(inv.str("currency_code", "currency"), "")
		state.CouponCode = inv.str("coupon_code", "coupon")
		if amt, err := money.ParseMajor(inv.raw("total", "amount", "net_amount", "total_amount")); err == nil {
			state.Amount = amt
		}
	}

	// The transaction record carries the payment identifier and, where exposed, the
	// applied coupon — both strengthen attribution to a local redemption.
	if ref.CustomerID != "" {
		q := url.Values{}
		if ref.InvoiceID != "" {
			q.Set("invoice_id", ref.InvoiceID)
		}
		if ref.Ref.ProductID != "" {
			q.Set("product_id", ref.Ref.ProductID)
		}
		if ref.Ref.PlanID != "" {
			q.Set("plan_id", ref.Ref.PlanID)
		}

		var raw json.RawMessage
		if err := c.get(ctx, "/transactions/"+url.PathEscape(ref.CustomerID), q, &raw); err != nil && !isNotFound(err) {
			return state, err
		} else if err == nil {
			for _, tx := range collection(raw, "transactions", "docs", "data", "result") {
				if ref.InvoiceID != "" && tx.str("invoice_id") != "" && tx.str("invoice_id") != ref.InvoiceID {
					continue
				}
				status := strings.ToLower(tx.str("status", "transaction_status"))
				if status == "success" || status == "successful" || status == "paid" {
					state.Paid = true
					state.Failed = false
				} else if status == "failed" || status == "failure" {
					state.Failed = true
				}
				state.TransactionID = firstNonEmpty(tx.str("_id", "id", "transaction_id", "payment_id"), state.TransactionID)
				state.ProductID = firstNonEmpty(state.ProductID, tx.str("product_id"))
				state.PlanID = firstNonEmpty(state.PlanID, tx.str("plan_id"))
				state.CouponCode = firstNonEmpty(state.CouponCode, tx.str("coupon_code", "coupon"))
				state.Currency = firstNonEmpty(state.Currency, tx.str("currency_code", "currency"))
				if state.Amount == 0 {
					if amt, err := money.ParseMajor(tx.raw("amount", "total", "net_amount")); err == nil {
						state.Amount = amt
					}
				}
				break
			}
		}
	}

	if state.Currency == "" {
		state.Currency = "INR"
	}
	return state, nil
}

// VerifyRefund confirms a refund against Pabbly before any coin is restored.
func (c *Client) VerifyRefund(ctx context.Context, ref provider.RefundRef) (provider.RefundState, error) {
	if ref.CustomerID == "" {
		return provider.RefundState{}, fmt.Errorf("%w: refund verification needs a customer id", provider.ErrNotFound)
	}
	q := url.Values{"type": {"refund"}}
	if ref.InvoiceID != "" {
		q.Set("invoice_id", ref.InvoiceID)
	}

	var raw json.RawMessage
	if err := c.get(ctx, "/transactions/"+url.PathEscape(ref.CustomerID), q, &raw); err != nil {
		return provider.RefundState{}, err
	}
	for _, tx := range collection(raw, "transactions", "docs", "data", "result") {
		if ref.PaymentID != "" && tx.str("payment_id", "parent_payment_id") != "" &&
			tx.str("payment_id", "parent_payment_id") != ref.PaymentID {
			continue
		}
		status := strings.ToLower(tx.str("status", "transaction_status"))
		if status == "success" || status == "successful" || status == "refunded" {
			amt, _ := money.ParseMajor(tx.raw("amount", "total", "refund_amount"))
			return provider.RefundState{
				Refunded: true,
				Amount:   amt,
				Currency: currencyOr(tx.str("currency_code", "currency"), "INR"),
			}, nil
		}
	}
	return provider.RefundState{}, nil
}

// RequestRefund asks Pabbly to refund a payment. Transactions are never deleted.
func (c *Client) RequestRefund(ctx context.Context, paymentID string) error {
	return c.post(ctx, "/transaction/refund/"+url.PathEscape(paymentID), map[string]any{}, nil)
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), provider.ErrNotFound.Error())
}
