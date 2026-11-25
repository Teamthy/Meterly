package memstore

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/powerdesk"
	"github.com/teamthy/meterly/internal/store"
)

type powerDeskRepo struct{ *repos }

func cloneOrg(o *powerdesk.Organisation) *powerdesk.Organisation {
	cp := *o
	cp.CreatedAt = o.CreatedAt.UTC()
	cp.UpdatedAt = o.UpdatedAt.UTC()
	if o.ManagerApprovalThresholdMinor != nil {
		v := *o.ManagerApprovalThresholdMinor
		cp.ManagerApprovalThresholdMinor = &v
	}
	return &cp
}

func cloneMember(m *powerdesk.Member) *powerdesk.Member {
	cp := *m
	cp.CreatedAt = m.CreatedAt.UTC()
	return &cp
}

func cloneProperty(p *powerdesk.Property) *powerdesk.Property {
	cp := *p
	cp.CreatedAt = p.CreatedAt.UTC()
	return &cp
}

func cloneBuilding(b *powerdesk.Building) *powerdesk.Building {
	cp := *b
	cp.CreatedAt = b.CreatedAt.UTC()
	return &cp
}

func cloneUnit(u *powerdesk.Unit) *powerdesk.Unit {
	cp := *u
	cp.CreatedAt = u.CreatedAt.UTC()
	return &cp
}

func cloneUnitMeter(um *powerdesk.UnitMeter) *powerdesk.UnitMeter {
	cp := *um
	cp.CreatedAt = um.CreatedAt.UTC()
	cp.UpdatedAt = um.UpdatedAt.UTC()
	if um.VerifiedAt != nil {
		t := um.VerifiedAt.UTC()
		cp.VerifiedAt = &t
	}
	return &cp
}

func cloneBulkTopup(bt *powerdesk.BulkTopup) *powerdesk.BulkTopup {
	cp := *bt
	cp.CreatedAt = bt.CreatedAt.UTC()
	cp.UpdatedAt = bt.UpdatedAt.UTC()
	if bt.ApprovedAt != nil {
		t := bt.ApprovedAt.UTC()
		cp.ApprovedAt = &t
	}
	if bt.ContributionID != nil {
		id := *bt.ContributionID
		cp.ContributionID = &id
	}
	return &cp
}

func cloneBulkItem(it *powerdesk.BulkTopupItem) *powerdesk.BulkTopupItem {
	cp := *it
	cp.CreatedAt = it.CreatedAt.UTC()
	cp.UpdatedAt = it.UpdatedAt.UTC()
	if it.TransactionID != nil {
		id := *it.TransactionID
		cp.TransactionID = &id
	}
	if it.AllocationID != nil {
		id := *it.AllocationID
		cp.AllocationID = &id
	}
	return &cp
}

func memberKey(orgID uuid.UUID, userID string) string {
	return orgID.String() + ":" + userID
}

func bulkIdemKey(orgID uuid.UUID, key string) string {
	return orgID.String() + ":" + key
}

// ---- organisations ---------------------------------------------------------

func (r powerDeskRepo) CreateOrg(_ context.Context, org *powerdesk.Organisation) error {
	defer r.lock()()
	if org.ID == uuid.Nil {
		org.ID = uuid.New()
	}
	now := time.Now().UTC()
	if org.CreatedAt.IsZero() {
		org.CreatedAt = now
	}
	if org.UpdatedAt.IsZero() {
		org.UpdatedAt = org.CreatedAt
	}
	if org.VerificationStatus == "" {
		org.VerificationStatus = powerdesk.VerificationPending
	}
	if _, exists := r.s.orgs[org.ID]; exists {
		return store.ErrDuplicate
	}
	id := org.ID
	r.s.orgs[id] = cloneOrg(org)
	r.onUndo(func() { delete(r.s.orgs, id) })
	return nil
}

func (r powerDeskRepo) GetOrg(_ context.Context, id uuid.UUID) (*powerdesk.Organisation, error) {
	defer r.lock()()
	o, ok := r.s.orgs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneOrg(o), nil
}

func (r powerDeskRepo) GetOrgForUpdate(ctx context.Context, id uuid.UUID) (*powerdesk.Organisation, error) {
	return r.GetOrg(ctx, id)
}

func (r powerDeskRepo) UpdateOrg(_ context.Context, org *powerdesk.Organisation) error {
	defer r.lock()()
	prev, ok := r.s.orgs[org.ID]
	if !ok {
		return store.ErrNotFound
	}
	prevCopy := cloneOrg(prev)
	id := org.ID
	r.s.orgs[id] = cloneOrg(org)
	r.onUndo(func() { r.s.orgs[id] = prevCopy })
	return nil
}

// ---- members ---------------------------------------------------------------

func (r powerDeskRepo) UpsertMember(_ context.Context, m *powerdesk.Member) error {
	defer r.lock()()
	if _, ok := r.s.orgs[m.OrganisationID]; !ok {
		return store.ErrNotFound
	}
	if !r.s.userIDs[m.UserID] {
		return store.ErrNotFound
	}
	if !m.Role.Valid() {
		return powerdesk.ErrInvalidRole
	}
	now := time.Now().UTC()
	key := memberKey(m.OrganisationID, m.UserID)
	if prev, ok := r.s.orgMembers[key]; ok {
		prevCopy := cloneMember(prev)
		updated := cloneMember(prev)
		updated.Role = m.Role
		updated.Status = powerdesk.MemberActive
		r.s.orgMembers[key] = updated
		*m = *cloneMember(updated)
		r.onUndo(func() { r.s.orgMembers[key] = prevCopy })
		return nil
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.Status = powerdesk.MemberActive
	r.s.orgMembers[key] = cloneMember(m)
	r.onUndo(func() { delete(r.s.orgMembers, key) })
	return nil
}

func (r powerDeskRepo) GetActiveMemberRole(_ context.Context, orgID uuid.UUID, userID string) (powerdesk.Role, error) {
	defer r.lock()()
	m, ok := r.s.orgMembers[memberKey(orgID, userID)]
	if !ok || m.Status != powerdesk.MemberActive {
		return "", store.ErrNotFound
	}
	return m.Role, nil
}

func (r powerDeskRepo) GetMemberForUpdate(_ context.Context, orgID uuid.UUID, userID string) (*powerdesk.Member, error) {
	defer r.lock()()
	m, ok := r.s.orgMembers[memberKey(orgID, userID)]
	if !ok || m.Status != powerdesk.MemberActive {
		return nil, store.ErrNotFound
	}
	return cloneMember(m), nil
}

func (r powerDeskRepo) ListActiveMembers(_ context.Context, orgID uuid.UUID) ([]*powerdesk.Member, error) {
	defer r.lock()()
	out := []*powerdesk.Member{}
	for _, m := range r.s.orgMembers {
		if m.OrganisationID == orgID && m.Status == powerdesk.MemberActive {
			out = append(out, cloneMember(m))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (r powerDeskRepo) CountActiveOwners(_ context.Context, orgID uuid.UUID) (int, error) {
	defer r.lock()()
	n := 0
	for _, m := range r.s.orgMembers {
		if m.OrganisationID == orgID && m.Status == powerdesk.MemberActive && m.Role == powerdesk.RoleOwner {
			n++
		}
	}
	return n, nil
}

func (r powerDeskRepo) RemoveMember(_ context.Context, orgID uuid.UUID, userID string) error {
	defer r.lock()()
	key := memberKey(orgID, userID)
	prev, ok := r.s.orgMembers[key]
	if !ok || prev.Status != powerdesk.MemberActive {
		return store.ErrNotFound
	}
	prevCopy := cloneMember(prev)
	updated := cloneMember(prev)
	updated.Status = powerdesk.MemberRemoved
	r.s.orgMembers[key] = updated
	r.onUndo(func() { r.s.orgMembers[key] = prevCopy })
	return nil
}

// ---- properties, buildings, units ------------------------------------------

func (r powerDeskRepo) CreateProperty(_ context.Context, p *powerdesk.Property) error {
	defer r.lock()()
	if _, ok := r.s.orgs[p.OrganisationID]; !ok {
		return store.ErrNotFound
	}
	if p.OwnerUserID != "" && !r.s.userIDs[p.OwnerUserID] {
		return store.ErrNotFound
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.Country == "" {
		p.Country = "NG"
	}
	if _, exists := r.s.properties[p.ID]; exists {
		return store.ErrDuplicate
	}
	id := p.ID
	r.s.properties[id] = cloneProperty(p)
	r.onUndo(func() { delete(r.s.properties, id) })
	return nil
}

func (r powerDeskRepo) GetProperty(_ context.Context, id uuid.UUID) (*powerdesk.Property, error) {
	defer r.lock()()
	p, ok := r.s.properties[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneProperty(p), nil
}

func (r powerDeskRepo) ListPropertiesForOrg(_ context.Context, orgID uuid.UUID) ([]*powerdesk.Property, error) {
	defer r.lock()()
	out := []*powerdesk.Property{}
	for _, p := range r.s.properties {
		if p.OrganisationID == orgID {
			out = append(out, cloneProperty(p))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (r powerDeskRepo) EnsureDefaultBuilding(_ context.Context, propertyID uuid.UUID, name string, now time.Time) (*powerdesk.Building, error) {
	defer r.lock()()
	if _, ok := r.s.properties[propertyID]; !ok {
		return nil, store.ErrNotFound
	}
	if strings.TrimSpace(name) == "" {
		name = powerdesk.DefaultBuildingName
	}
	norm := strings.ToLower(strings.TrimSpace(name))
	for _, b := range r.s.buildings {
		if b.PropertyID == propertyID && strings.ToLower(b.Name) == norm {
			return cloneBuilding(b), nil
		}
	}
	b := &powerdesk.Building{
		ID:         uuid.New(),
		PropertyID: propertyID,
		Name:       strings.TrimSpace(name),
		CreatedAt:  now.UTC(),
	}
	id := b.ID
	r.s.buildings[id] = cloneBuilding(b)
	r.onUndo(func() { delete(r.s.buildings, id) })
	return cloneBuilding(b), nil
}

func (r powerDeskRepo) GetBuilding(_ context.Context, id uuid.UUID) (*powerdesk.Building, error) {
	defer r.lock()()
	b, ok := r.s.buildings[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneBuilding(b), nil
}

func (r powerDeskRepo) CreateUnit(_ context.Context, u *powerdesk.Unit) error {
	defer r.lock()()
	b, ok := r.s.buildings[u.BuildingID]
	if !ok {
		return store.ErrNotFound
	}
	p, ok := r.s.properties[b.PropertyID]
	if !ok {
		return store.ErrNotFound
	}
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	u.PropertyID = p.ID
	u.OrganisationID = p.OrganisationID
	if _, exists := r.s.units[u.ID]; exists {
		return store.ErrDuplicate
	}
	norm := strings.ToLower(strings.TrimSpace(u.UnitName))
	for _, existing := range r.s.units {
		if existing.BuildingID == u.BuildingID && strings.ToLower(existing.UnitName) == norm {
			return store.ErrDuplicate
		}
	}
	id := u.ID
	r.s.units[id] = cloneUnit(u)
	r.onUndo(func() { delete(r.s.units, id) })
	return nil
}

func (r powerDeskRepo) GetUnit(_ context.Context, id uuid.UUID) (*powerdesk.Unit, error) {
	defer r.lock()()
	u, ok := r.s.units[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneUnit(u), nil
}

func (r powerDeskRepo) ListUnitsForProperty(_ context.Context, propertyID uuid.UUID) ([]*powerdesk.Unit, error) {
	defer r.lock()()
	out := []*powerdesk.Unit{}
	for _, u := range r.s.units {
		if u.PropertyID == propertyID {
			out = append(out, cloneUnit(u))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (r powerDeskRepo) ListUnitsForOrg(_ context.Context, orgID uuid.UUID) ([]*powerdesk.Unit, error) {
	defer r.lock()()
	out := []*powerdesk.Unit{}
	for _, u := range r.s.units {
		if u.OrganisationID == orgID {
			out = append(out, cloneUnit(u))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// ---- unit_meters -----------------------------------------------------------

func (r powerDeskRepo) AttachUnitMeter(_ context.Context, um *powerdesk.UnitMeter) error {
	defer r.lock()()
	u, ok := r.s.units[um.UnitID]
	if !ok {
		return store.ErrNotFound
	}
	if _, ok := r.s.meters[um.MeterID]; !ok {
		return store.ErrNotFound
	}
	if !r.s.userIDs[um.CreatedByUserID] {
		return store.ErrNotFound
	}
	if um.ID == uuid.Nil {
		um.ID = uuid.New()
	}
	now := time.Now().UTC()
	if um.CreatedAt.IsZero() {
		um.CreatedAt = now
	}
	if um.UpdatedAt.IsZero() {
		um.UpdatedAt = um.CreatedAt
	}
	if um.Status == "" {
		um.Status = powerdesk.UnitMeterActive
	}
	um.OrganisationID = u.OrganisationID
	um.PropertyID = u.PropertyID
	if _, exists := r.s.unitMeters[um.ID]; exists {
		return store.ErrDuplicate
	}
	if um.Status == powerdesk.UnitMeterActive {
		for _, existing := range r.s.unitMeters {
			if existing.UnitID == um.UnitID && existing.Status == powerdesk.UnitMeterActive {
				return store.ErrDuplicate
			}
		}
	}
	id := um.ID
	r.s.unitMeters[id] = cloneUnitMeter(um)
	r.onUndo(func() { delete(r.s.unitMeters, id) })
	return nil
}

func (r powerDeskRepo) GetUnitMeter(_ context.Context, id uuid.UUID) (*powerdesk.UnitMeter, error) {
	defer r.lock()()
	um, ok := r.s.unitMeters[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneUnitMeter(um), nil
}

func (r powerDeskRepo) GetUnitMeterForUpdate(ctx context.Context, id uuid.UUID) (*powerdesk.UnitMeter, error) {
	return r.GetUnitMeter(ctx, id)
}

func (r powerDeskRepo) GetActiveUnitMeterByUnit(_ context.Context, unitID uuid.UUID) (*powerdesk.UnitMeter, error) {
	defer r.lock()()
	for _, um := range r.s.unitMeters {
		if um.UnitID == unitID && um.Status == powerdesk.UnitMeterActive {
			return cloneUnitMeter(um), nil
		}
	}
	return nil, store.ErrNotFound
}

func (r powerDeskRepo) ListUnitMetersForOrg(_ context.Context, orgID uuid.UUID) ([]*powerdesk.UnitMeter, error) {
	defer r.lock()()
	out := []*powerdesk.UnitMeter{}
	for _, um := range r.s.unitMeters {
		if um.OrganisationID == orgID && um.Status == powerdesk.UnitMeterActive {
			out = append(out, cloneUnitMeter(um))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (r powerDeskRepo) UpdateUnitMeter(_ context.Context, um *powerdesk.UnitMeter) error {
	defer r.lock()()
	prev, ok := r.s.unitMeters[um.ID]
	if !ok {
		return store.ErrNotFound
	}
	if um.VerifiedByUserID != "" && !r.s.userIDs[um.VerifiedByUserID] {
		return store.ErrNotFound
	}
	prevCopy := cloneUnitMeter(prev)
	id := um.ID
	r.s.unitMeters[id] = cloneUnitMeter(um)
	r.onUndo(func() { r.s.unitMeters[id] = prevCopy })
	return nil
}

// ---- bulk_topups & items ---------------------------------------------------

func (r powerDeskRepo) CreateBulkTopup(_ context.Context, bt *powerdesk.BulkTopup, items []*powerdesk.BulkTopupItem) error {
	defer r.lock()()
	if _, ok := r.s.pots[bt.PotID]; !ok {
		return store.ErrNotFound
	}
	if _, ok := r.s.orgs[bt.OrganisationID]; !ok {
		return store.ErrNotFound
	}
	if !r.s.userIDs[bt.CreatedByUserID] {
		return store.ErrNotFound
	}
	if _, exists := r.s.bulkTopups[bt.PotID]; exists {
		return store.ErrDuplicate
	}
	if bt.IdempotencyKey != "" {
		if _, exists := r.s.bulkTopupByIdem[bulkIdemKey(bt.OrganisationID, bt.IdempotencyKey)]; exists {
			return store.ErrDuplicate
		}
	}
	seenUnits := map[uuid.UUID]bool{}
	for _, it := range items {
		if it.ID == uuid.Nil {
			it.ID = uuid.New()
		}
		if _, exists := r.s.bulkItems[it.ID]; exists {
			return store.ErrDuplicate
		}
		if seenUnits[it.UnitID] {
			return store.ErrDuplicate
		}
		seenUnits[it.UnitID] = true
		if _, ok := r.s.units[it.UnitID]; !ok {
			return store.ErrNotFound
		}
		if _, ok := r.s.unitMeters[it.UnitMeterID]; !ok {
			return store.ErrNotFound
		}
		if _, ok := r.s.meters[it.MeterID]; !ok {
			return store.ErrNotFound
		}
	}
	potID := bt.PotID
	idemK := ""
	if bt.IdempotencyKey != "" {
		idemK = bulkIdemKey(bt.OrganisationID, bt.IdempotencyKey)
		r.s.bulkTopupByIdem[idemK] = potID
	}
	r.s.bulkTopups[potID] = cloneBulkTopup(bt)
	insertedItemIDs := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		id := it.ID
		r.s.bulkItems[id] = cloneBulkItem(it)
		insertedItemIDs = append(insertedItemIDs, id)
	}
	r.onUndo(func() {
		delete(r.s.bulkTopups, potID)
		if idemK != "" {
			delete(r.s.bulkTopupByIdem, idemK)
		}
		for _, id := range insertedItemIDs {
			delete(r.s.bulkItems, id)
		}
	})
	return nil
}

func (r powerDeskRepo) GetBulkTopup(_ context.Context, potID uuid.UUID) (*powerdesk.BulkTopup, error) {
	defer r.lock()()
	bt, ok := r.s.bulkTopups[potID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneBulkTopup(bt), nil
}

func (r powerDeskRepo) GetBulkTopupForUpdate(ctx context.Context, potID uuid.UUID) (*powerdesk.BulkTopup, error) {
	return r.GetBulkTopup(ctx, potID)
}

func (r powerDeskRepo) GetBulkTopupByIdempotency(_ context.Context, orgID uuid.UUID, key string) (*powerdesk.BulkTopup, error) {
	defer r.lock()()
	if key == "" {
		return nil, store.ErrNotFound
	}
	potID, ok := r.s.bulkTopupByIdem[bulkIdemKey(orgID, key)]
	if !ok {
		return nil, store.ErrNotFound
	}
	bt, ok := r.s.bulkTopups[potID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneBulkTopup(bt), nil
}

func (r powerDeskRepo) UpdateBulkTopup(_ context.Context, bt *powerdesk.BulkTopup) error {
	defer r.lock()()
	prev, ok := r.s.bulkTopups[bt.PotID]
	if !ok {
		return store.ErrNotFound
	}
	if bt.ApprovedByUserID != "" && !r.s.userIDs[bt.ApprovedByUserID] {
		return store.ErrNotFound
	}
	if bt.ContributionID != nil {
		if _, ok := r.s.contribs[*bt.ContributionID]; !ok {
			return store.ErrNotFound
		}
	}
	prevCopy := cloneBulkTopup(prev)
	potID := bt.PotID
	r.s.bulkTopups[potID] = cloneBulkTopup(bt)
	r.onUndo(func() { r.s.bulkTopups[potID] = prevCopy })
	return nil
}

func (r powerDeskRepo) ListBulkTopupItems(_ context.Context, potID uuid.UUID) ([]*powerdesk.BulkTopupItem, error) {
	defer r.lock()()
	out := []*powerdesk.BulkTopupItem{}
	for _, it := range r.s.bulkItems {
		if it.PotID == potID {
			out = append(out, cloneBulkItem(it))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (r powerDeskRepo) UpdateBulkTopupItem(_ context.Context, item *powerdesk.BulkTopupItem) error {
	defer r.lock()()
	prev, ok := r.s.bulkItems[item.ID]
	if !ok {
		return store.ErrNotFound
	}
	if item.TransactionID != nil {
		if _, ok := r.s.txns[*item.TransactionID]; !ok {
			return store.ErrNotFound
		}
	}
	if item.AllocationID != nil {
		if _, ok := r.s.allocs[*item.AllocationID]; !ok {
			return store.ErrNotFound
		}
	}
	prevCopy := cloneBulkItem(prev)
	id := item.ID
	r.s.bulkItems[id] = cloneBulkItem(item)
	r.onUndo(func() { r.s.bulkItems[id] = prevCopy })
	return nil
}

func (r powerDeskRepo) ListBulkTopupItemsForOrg(_ context.Context, orgID uuid.UUID, propertyID *uuid.UUID) ([]*powerdesk.BulkTopupItem, error) {
	defer r.lock()()
	out := []*powerdesk.BulkTopupItem{}
	for _, it := range r.s.bulkItems {
		if it.OrganisationID != orgID {
			continue
		}
		if propertyID != nil && it.PropertyID != *propertyID {
			continue
		}
		out = append(out, cloneBulkItem(it))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}
