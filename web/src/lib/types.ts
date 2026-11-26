export type SupportMode = "AGENT" | "HUMAN";
export type SupportStatus =
  | "OPEN"
  | "WAITING_HUMAN"
  | "ASSIGNED"
  | "RESOLVED"
  | "CLOSED";
export type SupportSender = "CUSTOMER" | "OPERATOR" | "AGENT" | "SYSTEM";

export interface SupportIdentityKey {
  user_id: string;
  public_key: string;
  algorithm: string;
  staff: boolean;
}

export interface SupportThread {
  id: string;
  customer_id: string;
  mode: SupportMode;
  status: SupportStatus;
  assigned_operator_id?: string;
  e2e: boolean;
  created_at: string;
  updated_at: string;
  last_message_at: string;
}

export interface SupportMessage {
  id: string;
  thread_id: string;
  sender_id: string;
  sender_kind: SupportSender;
  e2e: boolean;
  ciphertext?: string;
  nonce?: string;
  body?: string;
  created_at: string;
}

export interface SupportThreadDetail {
  thread: SupportThread;
  wrapped_keys?: Array<{ holder_user_id: string; wrapped_key: string; nonce: string }>;
  me?: string;
  customer_public_key?: string;
  assigned_operator_public_key?: string;
  message?: SupportMessage;
}

export interface Capability {
  supported: boolean;
  authorised: boolean;
  reachable: boolean | null;
  confirmed: boolean;
  last_verified_at?: string | null;
}

export interface Capabilities {
  token_vending: Capability;
  remote_credit: Capability;
  remote_reading: Capability;
}

export interface Meter {
  meter_id: string;
  masked_meter_number: string;
  disco: string;
  meter_type: string;
  customer_name: string;
  address?: string;
  validated_at: string;
  capabilities: Capabilities;
  label?: string;
  permission?: "VIEW" | "PURCHASE" | "MANAGE";
}

export interface MeterConsumption {
  kwh_per_hour: number;
  kwh_per_day: number;
  window_hours: number;
  samples: number;
  computed_at: string;
}

export interface MeterLastCredit {
  units_kwh: number;
  credited_at: string;
  delivery_method: string;
  transaction_id: string;
}

export interface MeterUsage {
  meter_id: string;
  remaining_kwh: number | null;
  remaining_source: "remote_reading" | "unavailable" | string;
  read_at?: string;
  provider?: string;
  confirmed: boolean;
  consumption: MeterConsumption | null;
  estimated_empty_at?: string | null;
  estimated_hours_remaining?: number | null;
  last_credit: MeterLastCredit | null;
  unavailable_reason?: string;
}

export interface MeterValidationResult {
  valid: boolean;
  validation_status: string;
  masked_meter_number: string;
  disco: string;
  meter_type: string;
  customer_name: string;
  capabilities: Capabilities;
}

export interface Money {
  Minor: number;
  Currency: string;
}

export interface Quote {
  ID?: string;
  quote_id?: string;
  MeterID?: string;
  meter_id?: string;
  Amount?: Money;
  amount_minor?: number;
  Fee?: Money;
  fee_minor?: number;
  Total?: Money;
  total_minor?: number;
  Currency?: string;
  currency?: string;
  EstimatedUnitsKWh?: number;
  estimated_units_kwh?: number;
  DeliveryMethod?: "TOKEN" | "REMOTE";
  delivery_method?: "TOKEN" | "REMOTE";
  ExpiresAt?: string;
  expires_at?: string;
}

export interface StateTransition {
  Dimension?: string;
  dimension?: string;
  From?: string;
  from?: string;
  To?: string;
  to?: string;
  Reason?: string;
  reason?: string;
  At?: string;
  at?: string;
}

export interface TransactionView {
  transaction_id: string;
  status: string;
  payment_status: string;
  vending_status: string;
  delivery_status: string;
  delivery_method: "TOKEN" | "REMOTE";
  meter_id: string;
  is_gift: boolean;
  recipient_name?: string;
  amount_minor: number;
  fee_minor: number;
  total_minor: number;
  currency: string;
  units_kwh: number;
  failure_code?: string;
  review_reason?: string;
  checkout_url?: string;
  history?: StateTransition[];
  created_at: string;
  updated_at: string;
  // Admin extra fields
  customer_id?: string;
  check_attempts?: number;
  payment_provider?: string;
  payment_reference?: string;
  vending_provider?: string;
  vending_reference?: string;
  delivery_provider?: string;
  delivery_reference?: string;
  refund_reference?: string;
  refund_amount_minor?: number;
  token_available?: boolean;
  credit_on_meter?: boolean;
}

/** Operator-attested AMI group (DisCo + digit prefix). Not a live HES session. */
export interface RemoteCohort {
  id: string;
  disco: string;
  number_prefix: string;
  manufacturer?: string;
  authorised: boolean;
  evidence: string;
  created_by: string;
  created_at: string;
  revoked_at?: string;
}

export type HouseholdRole =
  | "OWNER"
  | "ADMINISTRATOR"
  | "MEMBER"
  | "VIEWER"
  | "RECIPIENT";

export type HouseholdMemberStatus = "INVITED" | "ACTIVE" | "DECLINED" | "REVOKED";

export interface Household {
  household_id: string;
  owner_user_id: string;
  name: string;
  created_at: string;
  updated_at: string;
}

export interface HouseholdMember {
  member_id: string;
  household_id: string;
  user_id: string;
  role: HouseholdRole;
  status: HouseholdMemberStatus;
  invited_by?: string;
  invited_at: string;
  accepted_at?: string;
  revoked_at?: string;
}

export interface HouseholdMeterLink {
  household_id: string;
  meter_id: string;
  added_by: string;
  created_at: string;
}

export interface HouseholdView {
  household: Household;
  members: HouseholdMember[];
  meters: HouseholdMeterLink[];
  my_role?: HouseholdRole;
  my_status?: HouseholdMemberStatus;
}

export interface MeterOwnership {
  ownership_id: string;
  meter_id: string;
  user_id: string;
  method: "ACCOUNT_NAME_MATCH" | "OPERATOR_ATTESTATION";
  reason?: string;
  verified_by: string;
  verified_at: string;
  revoked_at?: string;
}

export interface TokenShare {
  share_id: string;
  transaction_id: string;
  channel: "COPY" | "SMS" | "EMAIL" | "WHATSAPP";
  recorded_at: string;
  destination_masked?: string;
}

export interface TokenRevealResponse {
  transaction_id: string;
  token: string;
  units_kwh: number;
  masked_meter_number: string;
  note: string;
}

export interface NotificationPreferences {
  sms: boolean;
  email: boolean;
  whatsapp: boolean;
  updated_at?: string;
}

export interface PayRequestView {
  id: string;
  meter_id: string;
  masked_meter_number: string;
  disco: string;
  amount_mode: "FIXED" | "RANGE";
  amount_minor?: number;
  min_amount_minor?: number;
  max_amount_minor?: number;
  fee_minor: number;
  currency: string;
  note?: string;
  status: "OPEN" | "PAID" | "EXPIRED" | "CANCELLED";
  max_payments: number;
  payments_count: number;
  expires_at: string;
  created_at: string;
  code?: string;
  share_url?: string;
  qr_png_base64?: string;
}

export interface PublicPayLinkView {
  requester_display_name: string;
  masked_meter_number: string;
  disco: string;
  amount_mode: "FIXED" | "RANGE";
  amount_minor?: number;
  min_amount_minor?: number;
  max_amount_minor?: number;
  fee_minor: number;
  currency: string;
  note?: string;
  status: "OPEN" | "PAID" | "EXPIRED" | "CANCELLED";
  max_payments: number;
  payments_count: number;
  expires_at: string;
}

export interface PublicPayStatusView {
  status: string;
  payment_status: string;
  vending_status: string;
  delivery_status: string;
  delivery_method: string;
  receipt: Record<string, unknown>;
}

export interface PotContributorOwnerView {
  id: string;
  initials: string;
  amount_minor: number;
  retained_minor: number;
  refund_amount_minor: number;
  status: string;
  paid_at?: string;
}

export interface CreatedPot {
  id: string;
  kind: string;
  meter_id?: string;
  masked_meter_number?: string;
  disco?: string;
  target_minor: number;
  electricity_amount_minor: number;
  fee_minor: number;
  funded_minor: number;
  remaining_minor: number;
  paid_count: number;
  suggested_shares?: number;
  suggested_share_minor?: number;
  currency: string;
  note?: string;
  status: string;
  deadline: string;
  transaction_id?: string;
  contributors?: PotContributorOwnerView[];
  code?: string;
  share_url?: string;
  qr_png_base64?: string;
}

export interface PublicPotView {
  organiser_display_name: string;
  masked_meter_number: string;
  disco: string;
  target_minor: number;
  electricity_amount_minor: number;
  fee_minor: number;
  funded_minor: number;
  remaining_minor: number;
  paid_count: number;
  suggested_shares?: number;
  suggested_share_minor?: number;
  currency: string;
  note?: string;
  status: string;
  deadline: string;
  contributors: Array<{
    initials: string;
    amount_minor: number;
    retained_minor: number;
    status: string;
    paid_at?: string;
  }>;
}

export interface SavedPaymentMethod {
  id: string;
  brand: string;
  last4: string;
  exp_month: number;
  exp_year: number;
  bank?: string;
  status: string;
  consent_version: string;
  consented_at: string;
  created_at: string;
}

export interface AutoPowerRun {
  id: string;
  rule_id: string;
  trigger_type: string;
  scheduled_for: string;
  amount_minor: number;
  currency: string;
  status: string;
  skip_reason?: string;
  failure_reason?: string;
  transaction_id?: string;
  created_at: string;
}

export interface AutoPowerRule {
  rule_id: string;
  meter_id: string;
  masked_meter_number?: string;
  payment_method_id: string;
  trigger_type: "SCHEDULED" | "LOW_BALANCE";
  amount_minor: number;
  monthly_cap_minor: number;
  currency: string;
  delivery_preference: string;
  frequency?: "WEEKLY" | "MONTHLY";
  days_of_week?: number[];
  days_of_month?: number[];
  time_of_day?: string;
  timezone: string;
  ends_at?: string;
  low_balance_threshold_kwh?: number;
  status: "ACTIVE" | "PAUSED" | "STOPPED";
  pause_reason?: string;
  consecutive_failures: number;
  next_scheduled_at?: string;
  last_reading_kwh?: number;
  upcoming_run?: AutoPowerRun;
  recent_runs?: AutoPowerRun[];
}

export interface OrgMember {
  id: string;
  organisation_id: string;
  user_id: string;
  role: "OWNER" | "MANAGER" | "VIEWER";
  status: string;
  created_at: string;
}

export interface Property {
  id: string;
  organisation_id: string;
  name: string;
  address?: string;
  city?: string;
  state?: string;
  country: string;
  created_at: string;
}

export interface UnitMeter {
  id: string;
  unit_id: string;
  meter_id: string;
  status: string;
  verified: boolean;
  verification_method?: string;
  evidence_reference?: string;
  disco_account_name?: string;
  verification_reason?: string;
}

export interface UnitView {
  id: string;
  organisation_id: string;
  property_id: string;
  building_id: string;
  unit_name: string;
  occupancy_status: string;
  unit_meter?: UnitMeter;
  masked_meter_number?: string;
  disco?: string;
}

export interface OrgDetailView {
  id: string;
  name: string;
  legal_name?: string;
  registration_number?: string;
  organisation_type: "LANDLORD" | "ESTATE" | "PROPERTY_MANAGER";
  verification_status: string;
  manager_approval_threshold_minor?: number;
  caller_role: "OWNER" | "MANAGER" | "VIEWER";
  members: OrgMember[];
  properties: Property[];
  units: UnitView[];
  created_at: string;
}

export interface BulkTopupItemView {
  id: string;
  property_id: string;
  property_name: string;
  unit_id: string;
  unit_name: string;
  masked_meter_number: string;
  disco: string;
  amount_minor: number;
  fee_minor: number;
  total_minor: number;
  currency: string;
  status: "PENDING" | "DELIVERED" | "FAILED_REFUNDED";
  transaction_id?: string;
  overall_status?: string;
  delivery_method?: string;
  token_available: boolean;
  failure_reason?: string;
}

export interface BulkTopupDetailView {
  pot_id: string;
  kind: string;
  organisation_id: string;
  pot_status: string;
  approval_status: string;
  creator_role: string;
  electricity_total_minor: number;
  fee_total_minor: number;
  total_minor: number;
  funded_minor: number;
  refunded_minor: number;
  currency: string;
  checkout_url?: string;
  payment_reference?: string;
  delivered_count: number;
  failed_refunded_count: number;
  pending_count: number;
  summary: string;
  items: BulkTopupItemView[];
  created_at: string;
}

export interface MonthlyStatement {
  organisation_id: string;
  organisation_name: string;
  month: string;
  timezone: string;
  description: string;
  electricity_purchased_minor: number;
  fees_minor: number;
  refunds_minor: number;
  net_charged_minor: number;
  currency: string;
  vended_count: number;
  refunded_count: number;
  properties: Array<{
    property_id: string;
    property_name: string;
    description: string;
    electricity_purchased_minor: number;
    fees_minor: number;
    refunds_minor: number;
    net_charged_minor: number;
    vended_count: number;
    refunded_count: number;
    units: Array<{
      unit_id: string;
      unit_name: string;
      masked_meter_number: string;
      disco: string;
      electricity_purchased_minor: number;
      fees_minor: number;
      refunds_minor: number;
      net_charged_minor: number;
      vended_count: number;
      refunded_count: number;
    }>;
  }>;
}

export type OnboardingStep =
  | "welcome"
  | "account"
  | "profile"
  | "meter"
  | "alerts"
  | "done";

export interface UserProfile {
  user_id: string;
  external_id: string;
  display_name: string;
  email_masked: string;
  phone_masked: string;
  onboarding_step: OnboardingStep;
  onboarding_completed_at?: string | null;
  created_at: string;
}

export interface Passkey {
  id: string;
  name: string;
  created_at: string;
  last_used_at: string;
  sign_count: number;
}
