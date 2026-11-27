package privacy

// Columns is the machine-readable data map. A test fails if migrations add a
// column named like email/phone/name/address/ip/msisdn without an entry.
func Columns() []Column {
	return []Column{
		{Table: "auth_sessions", Column: "ip_hmac", Personal: true, What: "HMAC of client IP bound to a passkey session", Purpose: "session binding / step-up", Basis: BasisContract, Retention: "90 days after session expiry", Storage: StorageHMAC},
		{Table: "beneficiaries", Column: "display_name", Personal: true, What: "Label the customer gave a saved beneficiary", Purpose: "UX", Basis: BasisContract, Retention: "until the beneficiary is removed or the account is erased", Storage: StoragePlaintext},
		{Table: "buildings", Column: "name", Personal: true, What: "Building name on a landlord property", Purpose: "Power Desk", Basis: BasisContract, Retention: "while the organisation exists; anonymise on erasure", Storage: StoragePlaintext},
		{Table: "channel_otps", Column: "phone_hmac", Personal: true, What: "HMAC of the phone that received an OTP", Purpose: "USSD/WhatsApp link verification", Basis: BasisContract, Retention: "7 days", Storage: StorageHMAC},
		{Table: "conversation_sessions", Column: "phone_hmac", Personal: true, What: "HMAC of the phone in an offline conversation", Purpose: "USSD/WhatsApp session", Basis: BasisContract, Retention: "session TTL then delete", Storage: StorageHMAC},
		{Table: "distribution_companies", Column: "name", Personal: false, What: "Disco display name (reference data)", Purpose: "catalog", Basis: BasisUnused, Retention: "indefinite (not personal)", Storage: StoragePlaintext},
		{Table: "gift_transactions", Column: "recipient_name", Personal: true, What: "Purchaser-supplied gift recipient label", Purpose: "gift UX", Basis: BasisConsent, Retention: "financial period (7 years) then anonymise", Storage: StoragePlaintext},
		{Table: "households", Column: "name", Personal: true, What: "Household display name", Purpose: "household sharing", Basis: BasisContract, Retention: "until household deleted or account erased", Storage: StoragePlaintext},
		{Table: "ledger_accounts", Column: "account_name", Personal: false, What: "Chart-of-accounts label", Purpose: "bookkeeping", Basis: BasisUnused, Retention: "indefinite (not personal)", Storage: StoragePlaintext},
		{Table: "meter_manufacturers", Column: "name", Personal: false, What: "Manufacturer reference", Purpose: "catalog", Basis: BasisUnused, Retention: "indefinite (not personal)", Storage: StoragePlaintext},
		{Table: "meter_models", Column: "model_name", Personal: false, What: "Meter model reference", Purpose: "catalog", Basis: BasisUnused, Retention: "indefinite (not personal)", Storage: StoragePlaintext},
		{Table: "meters", Column: "address", Personal: true, What: "Service address returned by meter verify", Purpose: "show the customer the right meter", Basis: BasisContract, Retention: "while the meter is linked; anonymise with the account", Storage: StoragePlaintext},
		{Table: "meters", Column: "customer_name", Personal: true, What: "Name on the Disco account from verify", Purpose: "meter identity / ownership attest", Basis: BasisContract, Retention: "while the meter is linked; anonymise with the account", Storage: StoragePlaintext},
		{Table: "notification_deliveries", Column: "provider_name", Personal: false, What: "Which SMS/email/WhatsApp adapter sent the job", Purpose: "ops", Basis: BasisUnused, Retention: "with the delivery row", Storage: StoragePlaintext},
		{Table: "offline_purchases", Column: "phone_hmac", Personal: true, What: "HMAC of the phone that bought over USSD/WhatsApp", Purpose: "daily channel caps", Basis: BasisContract, Retention: "financial period then anonymise", Storage: StorageHMAC},
		{Table: "offline_token_resends", Column: "phone_hmac", Personal: true, What: "HMAC of the phone a token was resent to", Purpose: "resend audit / caps", Basis: BasisContract, Retention: "financial period then anonymise", Storage: StorageHMAC},
		{Table: "organisations", Column: "legal_name", Personal: true, What: "Landlord organisation legal name", Purpose: "Power Desk KYB", Basis: BasisContract, Retention: "while the org exists; anonymise on erasure", Storage: StoragePlaintext},
		{Table: "organisations", Column: "name", Personal: true, What: "Landlord organisation display name", Purpose: "Power Desk", Basis: BasisContract, Retention: "while the org exists; anonymise on erasure", Storage: StoragePlaintext},
		{Table: "phone_channel_links", Column: "phone_hmac", Personal: true, What: "HMAC binding a phone to a user", Purpose: "offline channel login", Basis: BasisContract, Retention: "until unlinked or account erased", Storage: StorageHMAC},
		{Table: "pot_contributions", Column: "email_ciphertext", Personal: true, What: "Guest contributor email, encrypted", Purpose: "settlement SMS/email, receipts", Basis: BasisConsent, Retention: "financial period then anonymise", Storage: StorageEncrypted},
		{Table: "pot_contributions", Column: "email_key_version", Personal: false, What: "Key version for contributor email", Purpose: "crypto", Basis: BasisUnused, Retention: "with the ciphertext", Storage: StoragePlaintext},
		{Table: "pot_contributions", Column: "name_ciphertext", Personal: true, What: "Guest contributor display name, encrypted", Purpose: "pot UX (initials only on public page)", Basis: BasisConsent, Retention: "financial period then anonymise", Storage: StorageEncrypted},
		{Table: "pot_contributions", Column: "name_key_version", Personal: false, What: "Key version for contributor name", Purpose: "crypto", Basis: BasisUnused, Retention: "with the ciphertext", Storage: StoragePlaintext},
		{Table: "pot_contributions", Column: "payment_provider_name", Personal: false, What: "PSP name for the contribution", Purpose: "ops", Basis: BasisUnused, Retention: "financial period", Storage: StoragePlaintext},
		{Table: "pot_contributions", Column: "phone_ciphertext", Personal: true, What: "Guest contributor phone, encrypted", Purpose: "optional settlement SMS", Basis: BasisConsent, Retention: "financial period then anonymise", Storage: StorageEncrypted},
		{Table: "pot_contributions", Column: "phone_key_version", Personal: false, What: "Key version for contributor phone", Purpose: "crypto", Basis: BasisUnused, Retention: "with the ciphertext", Storage: StoragePlaintext},
		{Table: "processed_channel_messages", Column: "phone_hmac", Personal: true, What: "HMAC of the inbound-message sender", Purpose: "webhook idempotency", Basis: BasisLegitimateInterest, Retention: "30 days", Storage: StorageHMAC},
		{Table: "properties", Column: "address", Personal: true, What: "Landlord property address", Purpose: "Power Desk", Basis: BasisContract, Retention: "while the property exists; anonymise on erasure", Storage: StoragePlaintext},
		{Table: "properties", Column: "name", Personal: true, What: "Landlord property name", Purpose: "Power Desk", Basis: BasisContract, Retention: "while the property exists; anonymise on erasure", Storage: StoragePlaintext},
		{Table: "provider_integrations", Column: "integration_name", Personal: false, What: "Internal integration label", Purpose: "ops", Basis: BasisUnused, Retention: "indefinite (not personal)", Storage: StoragePlaintext},
		{Table: "providers", Column: "name", Personal: false, What: "Payment/vending provider code", Purpose: "ops", Basis: BasisUnused, Retention: "indefinite (not personal)", Storage: StoragePlaintext},
		{Table: "risk_decisions", Column: "email_hmac", Personal: true, What: "HMAC of email at risk evaluation", Purpose: "abuse control", Basis: BasisLegitimateInterest, Retention: "90 days (observations); decisions kept with the txn", Storage: StorageHMAC},
		{Table: "risk_decisions", Column: "ip_hmac", Personal: true, What: "HMAC of IP at risk evaluation", Purpose: "abuse control", Basis: BasisLegitimateInterest, Retention: "with the decision row", Storage: StorageHMAC},
		{Table: "risk_observations", Column: "email_hmac", Personal: true, What: "HMAC of email on a checkout observation", Purpose: "velocity / denylist", Basis: BasisLegitimateInterest, Retention: "90 days", Storage: StorageHMAC},
		{Table: "risk_observations", Column: "ip_hmac", Personal: true, What: "HMAC of IP on a checkout observation", Purpose: "velocity", Basis: BasisLegitimateInterest, Retention: "90 days", Storage: StorageHMAC},
		{Table: "risk_observations", Column: "phone_hmac", Personal: true, What: "HMAC of phone on a checkout observation", Purpose: "velocity / denylist", Basis: BasisLegitimateInterest, Retention: "90 days", Storage: StorageHMAC},
		{Table: "roles", Column: "name", Personal: false, What: "RBAC role name", Purpose: "authz", Basis: BasisUnused, Retention: "indefinite (not personal)", Storage: StoragePlaintext},
		{Table: "saved_payment_methods", Column: "email_ciphertext", Personal: true, What: "Card-network email on a saved Paystack authorization, encrypted", Purpose: "AutoPower / saved card", Basis: BasisConsent, Retention: "until the method is deleted", Storage: StorageEncrypted},
		{Table: "security_events", Column: "ip_hmac", Personal: true, What: "HMAC of IP on a security event", Purpose: "security audit", Basis: BasisLegitimateInterest, Retention: "with the event row; no raw IP", Storage: StorageHMAC},
		{Table: "transaction_payers", Column: "email_ciphertext", Personal: true, What: "Pay-for-Me guest payer email, encrypted", Purpose: "receipt", Basis: BasisContract, Retention: "financial period then anonymise", Storage: StorageEncrypted},
		{Table: "transaction_payers", Column: "email_key_version", Personal: false, What: "Key version for payer email", Purpose: "crypto", Basis: BasisUnused, Retention: "with the ciphertext", Storage: StoragePlaintext},
		{Table: "transaction_payers", Column: "payer_name_ciphertext", Personal: true, What: "Pay-for-Me guest payer name, encrypted", Purpose: "receipt", Basis: BasisContract, Retention: "financial period then anonymise", Storage: StorageEncrypted},
		{Table: "transaction_payers", Column: "payer_name_key_version", Personal: false, What: "Key version for payer name", Purpose: "crypto", Basis: BasisUnused, Retention: "with the ciphertext", Storage: StoragePlaintext},
		{Table: "unit_meters", Column: "disco_account_name", Personal: true, What: "Name on the Disco account attached to a unit", Purpose: "Power Desk ownership match", Basis: BasisContract, Retention: "while attached; anonymise on erasure", Storage: StoragePlaintext},
		{Table: "units", Column: "unit_name", Personal: true, What: "Unit label (often an apartment number)", Purpose: "Power Desk", Basis: BasisContract, Retention: "while the unit exists; anonymise on erasure", Storage: StoragePlaintext},
		{Table: "users", Column: "display_name", Personal: true, What: "Chosen display name", Purpose: "profile / receipts", Basis: BasisContract, Retention: "until account erasure", Storage: StoragePlaintext},
		{Table: "users", Column: "email_ciphertext", Personal: true, What: "Verified email, encrypted and bound to user id", Purpose: "transactional notify, login", Basis: BasisContract, Retention: "until account erasure", Storage: StorageEncrypted},
		{Table: "users", Column: "email_hmac", Personal: true, What: "HMAC of normalised email for lookup / opt-out", Purpose: "lookup, STOP", Basis: BasisContract, Retention: "until account erasure", Storage: StorageHMAC},
		{Table: "users", Column: "email_key_version", Personal: false, What: "Key version for user email", Purpose: "crypto", Basis: BasisUnused, Retention: "with the ciphertext", Storage: StoragePlaintext},
		{Table: "users", Column: "email_masked", Personal: true, What: "Masked email for UI (a***@x.com)", Purpose: "profile", Basis: BasisContract, Retention: "until account erasure", Storage: StoragePlaintext},
		{Table: "users", Column: "first_name", Personal: true, What: "Unused leftover from 0001_init; never written after contacts moved to ciphertext", Purpose: "unused", Basis: BasisUnused, Retention: "delete on erasure; do not populate", Storage: StorageUnused},
		{Table: "users", Column: "last_name", Personal: true, What: "Unused leftover from 0001_init; never written after contacts moved to ciphertext", Purpose: "unused", Basis: BasisUnused, Retention: "delete on erasure; do not populate", Storage: StorageUnused},
		{Table: "users", Column: "phone_ciphertext", Personal: true, What: "Verified E.164 phone, encrypted and bound to user id", Purpose: "transactional notify, USSD link", Basis: BasisContract, Retention: "until account erasure", Storage: StorageEncrypted},
		{Table: "users", Column: "phone_hmac", Personal: true, What: "HMAC of normalised phone for lookup / opt-out", Purpose: "lookup, STOP", Basis: BasisContract, Retention: "until account erasure", Storage: StorageHMAC},
		{Table: "users", Column: "phone_key_version", Personal: false, What: "Key version for user phone", Purpose: "crypto", Basis: BasisUnused, Retention: "with the ciphertext", Storage: StoragePlaintext},
		{Table: "users", Column: "phone_masked", Personal: true, What: "Masked phone for UI", Purpose: "profile", Basis: BasisContract, Retention: "until account erasure", Storage: StoragePlaintext},
		{Table: "webauthn_credentials", Column: "name", Personal: true, What: "User-chosen passkey nickname", Purpose: "security UI", Basis: BasisContract, Retention: "until the credential is deleted", Storage: StoragePlaintext},
	}
}

func columnIndex() map[string]Column {
	m := make(map[string]Column, 64)
	for _, c := range Columns() {
		m[c.Key()] = c
	}
	return m
}
