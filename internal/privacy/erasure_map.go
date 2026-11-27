package privacy

const (
	EraseDelete    = "DELETE"
	EraseAnonymise = "ANONYMISE"
	EraseShared    = "SHARED"
	EraseKeep      = "KEEP"
)

// ErasureActions maps every personal column to what B3 does with it.
// SHARED: kept when another household still uses the row (meters).
// KEEP: not the data subject's identity (Power Desk org assets, other people's
// guest contacts, catalog).
func ErasureActions() map[string]string {
	return map[string]string{
		"auth_sessions.ip_hmac":                    EraseDelete,
		"beneficiaries.display_name":               EraseDelete,
		"buildings.name":                           EraseKeep,
		"channel_otps.phone_hmac":                  EraseDelete,
		"conversation_sessions.phone_hmac":         EraseDelete,
		"gift_transactions.recipient_name":         EraseAnonymise,
		"households.name":                          EraseAnonymise,
		"meters.address":                           EraseShared,
		"meters.customer_name":                     EraseShared,
		"offline_purchases.phone_hmac":             EraseAnonymise,
		"offline_token_resends.phone_hmac":         EraseAnonymise,
		"organisations.legal_name":                 EraseKeep,
		"organisations.name":                       EraseKeep,
		"phone_channel_links.phone_hmac":           EraseDelete,
		"pot_contributions.email_ciphertext":       EraseAnonymise,
		"pot_contributions.name_ciphertext":        EraseAnonymise,
		"pot_contributions.phone_ciphertext":       EraseAnonymise,
		"processed_channel_messages.phone_hmac":    EraseKeep,
		"properties.address":                       EraseKeep,
		"properties.name":                          EraseKeep,
		"risk_decisions.email_hmac":                EraseAnonymise,
		"risk_decisions.ip_hmac":                   EraseAnonymise,
		"risk_observations.email_hmac":             EraseAnonymise,
		"risk_observations.ip_hmac":                EraseAnonymise,
		"risk_observations.phone_hmac":             EraseAnonymise,
		"saved_payment_methods.email_ciphertext":   EraseDelete,
		"security_events.ip_hmac":                  EraseDelete,
		"transaction_payers.email_ciphertext":      EraseKeep,
		"transaction_payers.payer_name_ciphertext": EraseKeep,
		"unit_meters.disco_account_name":           EraseKeep,
		"units.unit_name":                          EraseKeep,
		"users.display_name":                       EraseDelete,
		"users.email_ciphertext":                   EraseDelete,
		"users.email_hmac":                         EraseDelete,
		"users.email_masked":                       EraseDelete,
		"users.first_name":                         EraseDelete,
		"users.last_name":                          EraseDelete,
		"users.phone_ciphertext":                   EraseDelete,
		"users.phone_hmac":                         EraseDelete,
		"users.phone_masked":                       EraseDelete,
		"webauthn_credentials.name":                EraseDelete,
	}
}
