package config

import (
	"github.com/stripe/stripe-go/v74"
)

type StripeConfig struct {
	SecretKey    string // Your Stripe API secret key
	PublishableKey string
	WebhookSecret string // For verifying webhook signatures
	APIVersion   string
}

func InitStripe(cfg StripeConfig) {
	stripe.Key = cfg.SecretKey
	stripe.SetAppInfo(&stripe.AppInfo{
		Name:    "bank-transfer-system",
		Version: "1.0.0",
	})
	
	if cfg.APIVersion != "" {
		stripe.SetApiVersion(cfg.APIVersion)
	}
}
