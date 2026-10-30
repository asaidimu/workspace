package memory

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/asaidimu/workspace/domain"
	"github.com/asaidimu/workspace/store"
)

// cloneConversation returns a copy with its slices and head copied, so a
// staged transaction cannot mutate committed state through a shared slice.
func cloneConversation(c domain.Conversation) domain.Conversation {
	if c.Topics != nil {
		c.Topics = append([]string(nil), c.Topics...)
	}
	if c.Attached != nil {
		c.Attached = append([]domain.ContentRef(nil), c.Attached...)
	}
	if c.PreferenceKeys != nil {
		c.PreferenceKeys = append([]domain.ContentRef(nil), c.PreferenceKeys...)
	}
	if c.Constraints != nil {
		c.Constraints = append(json.RawMessage(nil), c.Constraints...)
	}
	if c.Head != nil {
		h := *c.Head
		c.Head = &h
	}
	if c.ForkedFrom != nil {
		f := *c.ForkedFrom
		c.ForkedFrom = &f
	}
	return c
}

func cloneTurn(t domain.Turn) domain.Turn {
	if t.Parent != nil {
		p := *t.Parent
		t.Parent = &p
	}
	if t.Blocks != nil {
		t.Blocks = append([]domain.Block(nil), t.Blocks...)
	}
	return t
}

func cloneContent(c domain.Content) domain.Content {
	if c.Topics != nil {
		c.Topics = append([]string(nil), c.Topics...)
	}
	if c.Value != nil {
		c.Value = append(json.RawMessage(nil), c.Value...)
	}
	if c.Metadata != nil {
		md := make(map[string]string, len(c.Metadata))
		for k, v := range c.Metadata {
			md[k] = v
		}
		c.Metadata = md
	}
	return c
}

func cloneRole(r domain.Role) domain.Role {
	if r.PreferenceKeys != nil {
		r.PreferenceKeys = append([]domain.ContentRef(nil), r.PreferenceKeys...)
	}
	if r.Topics != nil {
		r.Topics = append([]string(nil), r.Topics...)
	}
	if r.Constraints != nil {
		r.Constraints = append(json.RawMessage(nil), r.Constraints...)
	}
	return r
}

func cloneBlob(b domain.Blob) domain.Blob {
	if b.Remotes != nil {
		rm := make(map[string]domain.RemoteRef, len(b.Remotes))
		for k, v := range b.Remotes {
			rm[k] = v
		}
		b.Remotes = rm
	}
	return b
}

func cloneRun(r domain.Run) domain.Run {
	if r.State != nil {
		r.State = append(json.RawMessage(nil), r.State...)
	}
	return r
}

func cloneReceipt(r domain.Receipt) domain.Receipt {
	if r.Result != nil {
		r.Result = append(json.RawMessage(nil), r.Result...)
	}
	return r
}

func cloneSettings(s domain.Settings) domain.Settings {
	if s.GlobalPreferenceKeys != nil {
		s.GlobalPreferenceKeys = append([]domain.ContentRef(nil), s.GlobalPreferenceKeys...)
	}
	return s
}

// --- pagination ---

// paginate sorts by sortKey ascending and returns one keyset page. Cursor is
// the last ID of the previous page, so paging is stable under concurrent
// inserts: a new record that sorts before the cursor is skipped rather than
// shifting the window.
func paginate[T any](items []T, key func(T) string, limit int, cursor string, order store.Order) store.Page[T] {
	sort.SliceStable(items, func(i, j int) bool {
		return key(items[i]) < key(items[j])
	})

	if cursor != "" {
		n := 0
		for _, it := range items {
			if key(it) > cursor {
				items[n] = it
				n++
			}
		}
		items = items[:n]
	}

	if limit <= 0 || limit > len(items) {
		limit = len(items)
	}
	page := store.Page[T]{Items: items[:limit]}
	if limit < len(items) {
		page.NextCursor = key(items[limit-1])
	}
	if order == store.NewestFirst {
		// Reverse in place after the window is chosen, so the cursor stays
		// the last (highest) key regardless of display order.
		for i, j := 0, len(page.Items)-1; i < j; i, j = i+1, j-1 {
			page.Items[i], page.Items[j] = page.Items[j], page.Items[i]
		}
	}
	return page
}

// topicsMatch reports whether a record's topics satisfy a TopicFilter. Any
// is a union, All an intersection, and both together are ANDed, per the
// store port. A record with no topics matches only an empty filter.
func topicsMatch(topics []string, f store.TopicFilter) bool {
	have := make(map[string]bool, len(topics))
	for _, t := range topics {
		have[t] = true
	}
	if len(f.Any) > 0 {
		ok := false
		for _, want := range f.Any {
			if have[want] {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, want := range f.All {
		if !have[want] {
			return false
		}
	}
	return true
}

// cloneIfNil returns a copy when the caller passed a value that must not
// alias the caller's slice after a commit.
func inScope(scope store.Scope, owner string) bool {
	if scope.System {
		return true
	}
	return scope.Owner == owner
}

// --- conversations ---

type conversationRepo struct {
	st    *Store
	scope store.Scope
	ds    *datastore
	open  func() error
}

func (r *conversationRepo) ready() error {
	if r.open != nil {
		return r.open()
	}
	return nil
}

// guard locks the store mutex unless this repo is transactional.
//
// A transactional repo runs inside Atomic, which already holds the store
// mutex for the whole callback; locking here would self-deadlock. A scoped
// repo runs unlocked, so it must take the mutex itself to keep the store's
// serializable guarantee.
func (r *conversationRepo) guard() {
	if r.open == nil {
		r.st.mu.Lock()
	}
}

// unguard releases the lock taken by guard, if any.
func (r *conversationRepo) unguard() {
	if r.open == nil {
		r.st.mu.Unlock()
	}
}

func (r *conversationRepo) Create(ctx context.Context, c domain.Conversation) (domain.Conversation, error) {
	if err := r.ready(); err != nil {
		return domain.Conversation{}, err
	}
	r.guard()
	defer r.unguard()
	if err := ctx.Err(); err != nil {
		return domain.Conversation{}, err
	}
	if c.ID == "" || !inScope(r.scope, c.Owner) {
		return domain.Conversation{}, store.ErrInvalid
	}
	if _, ok := r.ds.conversations[c.ID]; ok {
		return domain.Conversation{}, store.ErrAlreadyExists
	}
	now := r.st.now()
	c.CreatedAt = now
	c.UpdatedAt = now
	c.Rev = 1
	if c.Status == "" {
		c.Status = domain.StatusActive
	}
	stored := cloneConversation(c)
	r.ds.conversations[c.ID] = stored
	return cloneConversation(stored), nil
}

func (r *conversationRepo) Get(ctx context.Context, id string) (domain.Conversation, error) {
	if err := r.ready(); err != nil {
		return domain.Conversation{}, err
	}
	r.guard()
	defer r.unguard()
	c, ok := r.ds.conversations[id]
	if !ok {
		return domain.Conversation{}, store.ErrNotFound
	}
	return cloneConversation(c), nil
}

func (r *conversationRepo) List(ctx context.Context, f store.ConversationFilter, p store.PageRequest) (store.Page[domain.Conversation], error) {
	if err := r.ready(); err != nil {
		return store.Page[domain.Conversation]{}, err
	}
	r.guard()
	defer r.unguard()
	var out []domain.Conversation
	for _, c := range r.ds.conversations {
		if !inScope(r.scope, c.Owner) {
			continue
		}
		if len(f.IDs) > 0 && !containsString(f.IDs, c.ID) {
			continue
		}
		if !topicsMatch(c.Topics, f.Topics) {
			continue
		}
		if f.RoleID != "" && c.RoleID != f.RoleID {
			continue
		}
		if f.Status != "" && string(c.Status) != f.Status {
			continue
		}
		if f.UpdatedAfter != nil && !c.UpdatedAt.After(*f.UpdatedAfter) {
			continue
		}
		out = append(out, cloneConversation(c))
	}
	return paginate(out, func(c domain.Conversation) string { return c.ID }, p.Limit, p.Cursor, p.Order), nil
}

func (r *conversationRepo) Update(ctx context.Context, id string, expectedRev int64, patch store.ConversationPatch) (domain.Conversation, error) {
	if err := r.ready(); err != nil {
		return domain.Conversation{}, err
	}
	r.guard()
	defer r.unguard()
	c, ok := r.ds.conversations[id]
	if !ok {
		return domain.Conversation{}, store.ErrNotFound
	}
	if c.Rev != expectedRev {
		return domain.Conversation{}, store.ErrConflict
	}
	if patch.Title != nil {
		c.Title = *patch.Title
	}
	if patch.RoleID != nil {
		c.RoleID = *patch.RoleID
	}
	if patch.Topics != nil {
		c.Topics = append([]string(nil), (*patch.Topics)...)
	}
	if patch.Attached != nil {
		c.Attached = append([]domain.ContentRef(nil), (*patch.Attached)...)
	}
	if patch.PreferenceKeys != nil {
		c.PreferenceKeys = append([]domain.ContentRef(nil), (*patch.PreferenceKeys)...)
	}
	if patch.Constraints != nil {
		c.Constraints = append(json.RawMessage(nil), (*patch.Constraints)...)
	}
	if patch.Status != nil {
		c.Status = *patch.Status
	}
	c.Rev++
	c.UpdatedAt = r.st.now()
	r.ds.conversations[id] = c
	return cloneConversation(c), nil
}

func (r *conversationRepo) SetHead(ctx context.Context, id string, expectedRev int64, head *domain.TurnRef) (domain.Conversation, error) {
	if err := r.ready(); err != nil {
		return domain.Conversation{}, err
	}
	r.guard()
	defer r.unguard()
	c, ok := r.ds.conversations[id]
	if !ok {
		return domain.Conversation{}, store.ErrNotFound
	}
	if c.Rev != expectedRev {
		return domain.Conversation{}, store.ErrConflict
	}
	if head == nil {
		c.Head = nil
	} else {
		h := *head
		c.Head = &h
	}
	c.Rev++
	c.UpdatedAt = r.st.now()
	r.ds.conversations[id] = c
	return cloneConversation(c), nil
}

func (r *conversationRepo) Delete(ctx context.Context, id string, expectedRev int64) error {
	if err := r.ready(); err != nil {
		return err
	}
	r.guard()
	defer r.unguard()
	c, ok := r.ds.conversations[id]
	if !ok {
		return store.ErrNotFound
	}
	if c.Rev != expectedRev {
		return store.ErrConflict
	}
	delete(r.ds.conversations, id)
	return nil
}

// --- turns ---

type turnRepo struct {
	st    *Store
	scope store.Scope
	ds    *datastore
	open  func() error
}

func (r *turnRepo) ready() error {
	if r.open != nil {
		return r.open()
	}
	return nil
}

// guard locks the store mutex unless this repo is transactional.
//
// A transactional repo runs inside Atomic, which already holds the store
// mutex for the whole callback; locking here would self-deadlock. A scoped
// repo runs unlocked, so it must take the mutex itself to keep the store's
// serializable guarantee.
func (r *turnRepo) guard() {
	if r.open == nil {
		r.st.mu.Lock()
	}
}

// unguard releases the lock taken by guard, if any.
func (r *turnRepo) unguard() {
	if r.open == nil {
		r.st.mu.Unlock()
	}
}

func (r *turnRepo) Append(ctx context.Context, t domain.Turn) (domain.Turn, error) {
	if err := r.ready(); err != nil {
		return domain.Turn{}, err
	}
	r.guard()
	defer r.unguard()
	if err := ctx.Err(); err != nil {
		return domain.Turn{}, err
	}
	if t.ID == "" || t.Version < 1 || !inScope(r.scope, t.Owner) {
		return domain.Turn{}, store.ErrInvalid
	}
	ref := t.Ref()
	if _, ok := r.ds.turns[ref]; ok {
		return domain.Turn{}, store.ErrAlreadyExists
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = r.st.now()
	}
	stored := cloneTurn(t)
	r.ds.turns[ref] = stored
	return cloneTurn(stored), nil
}

func (r *turnRepo) Get(ctx context.Context, ref domain.TurnRef) (domain.Turn, error) {
	if err := r.ready(); err != nil {
		return domain.Turn{}, err
	}
	r.guard()
	defer r.unguard()
	t, ok := r.ds.turns[ref]
	if !ok {
		return domain.Turn{}, store.ErrNotFound
	}
	return cloneTurn(t), nil
}

func (r *turnRepo) Versions(ctx context.Context, id string) ([]domain.TurnRef, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	r.guard()
	defer r.unguard()
	var out []domain.TurnRef
	for ref := range r.ds.turns {
		if ref.ID == id {
			out = append(out, ref)
		}
	}
	sortRefs(out)
	return out, nil
}

func (r *turnRepo) Children(ctx context.Context, parent domain.TurnRef) ([]domain.TurnRef, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	r.guard()
	defer r.unguard()
	var out []domain.TurnRef
	for ref, t := range r.ds.turns {
		if t.Parent != nil && *t.Parent == parent {
			out = append(out, ref)
		}
	}
	sortRefs(out)
	return out, nil
}

// Chain walks parent links from head, collecting turns. Limit keeps only the
// newest Limit turns; Until stops before that ancestor, exclusive.
func (r *turnRepo) Chain(ctx context.Context, head domain.TurnRef, opts store.ChainOptions) ([]domain.Turn, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	r.guard()
	defer r.unguard()
	if _, ok := r.ds.turns[head]; !ok {
		return nil, store.ErrNotFound
	}
	seen := map[domain.TurnRef]bool{}
	var chain []domain.Turn
	cur := head
	for {
		if seen[cur] {
			break
		}
		seen[cur] = true
		t, ok := r.ds.turns[cur]
		if !ok {
			break
		}
		chain = append(chain, cloneTurn(t))
		if opts.Until != nil && *opts.Until == cur {
			break
		}
		if t.Parent == nil {
			break
		}
		cur = *t.Parent
	}
	if opts.Limit > 0 && len(chain) > opts.Limit {
		chain = chain[:opts.Limit]
	}
	return chain, nil
}

func sortRefs(refs []domain.TurnRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ID != refs[j].ID {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Version < refs[j].Version
	})
}

// --- content ---

type contentRepo struct {
	st    *Store
	scope store.Scope
	ds    *datastore
	open  func() error
}

func (r *contentRepo) ready() error {
	if r.open != nil {
		return r.open()
	}
	return nil
}

// guard locks the store mutex unless this repo is transactional.
//
// A transactional repo runs inside Atomic, which already holds the store
// mutex for the whole callback; locking here would self-deadlock. A scoped
// repo runs unlocked, so it must take the mutex itself to keep the store's
// serializable guarantee.
func (r *contentRepo) guard() {
	if r.open == nil {
		r.st.mu.Lock()
	}
}

// unguard releases the lock taken by guard, if any.
func (r *contentRepo) unguard() {
	if r.open == nil {
		r.st.mu.Unlock()
	}
}

// Put writes a new version of a content key. baseVersion is the version the
// caller based the write on; it must match the current head version, which is
// the compare-and-swap the port requires. baseVersion 0 means "create the
// first version".
func (r *contentRepo) Put(ctx context.Context, c domain.Content, baseVersion int) (domain.Content, error) {
	if err := r.ready(); err != nil {
		return domain.Content{}, err
	}
	r.guard()
	defer r.unguard()
	if c.Key == "" || !inScope(r.scope, c.Owner) {
		return domain.Content{}, store.ErrInvalid
	}
	if baseVersion < 0 {
		return domain.Content{}, store.ErrInvalid
	}
	head := r.headVersion(c.Key)
	if baseVersion == 0 {
		if head != 0 {
			return domain.Content{}, store.ErrAlreadyExists
		}
	} else if baseVersion != head {
		return domain.Content{}, store.ErrConflict
	}
	next := head + 1
	c.Version = next
	if c.CreatedAt.IsZero() {
		c.CreatedAt = r.st.now()
	}
	c.Tombstoned = false
	stored := cloneContent(c)
	r.ds.contents[c.Ref()] = stored
	return cloneContent(stored), nil
}

func (r *contentRepo) headVersion(key string) int {
	head := 0
	for ref := range r.ds.contents {
		if ref.Key != key {
			continue
		}
		if ref.Version > head {
			head = ref.Version
		}
	}
	return head
}

func (r *contentRepo) Get(ctx context.Context, ref domain.ContentRef) (domain.Content, error) {
	if err := r.ready(); err != nil {
		return domain.Content{}, err
	}
	r.guard()
	defer r.unguard()
	if ref.Version == 0 {
		v := r.headVersion(ref.Key)
		if v == 0 {
			return domain.Content{}, store.ErrNotFound
		}
		ref.Version = v
	}
	c, ok := r.ds.contents[ref]
	if !ok {
		return domain.Content{}, store.ErrNotFound
	}
	if c.Tombstoned {
		return domain.Content{}, store.ErrNotFound
	}
	return cloneContent(c), nil
}

func (r *contentRepo) List(ctx context.Context, f store.ContentFilter, p store.PageRequest) (store.Page[domain.Content], error) {
	if err := r.ready(); err != nil {
		return store.Page[domain.Content]{}, err
	}
	r.guard()
	defer r.unguard()
	headByKey := map[string]domain.Content{}
	for ref, c := range r.ds.contents {
		if !inScope(r.scope, c.Owner) {
			continue
		}
		if cur, ok := headByKey[ref.Key]; !ok || ref.Version > cur.Version {
			headByKey[ref.Key] = c
		}
	}

	var out []domain.Content
	for ref, c := range r.ds.contents {
		if !inScope(r.scope, c.Owner) {
			continue
		}
		if c.Tombstoned && !f.IncludeTombstoned {
			continue
		}
		if !f.AllVersions && headByKey[ref.Key].Version != ref.Version {
			continue
		}
		if len(f.Keys) > 0 && !containsString(f.Keys, ref.Key) {
			continue
		}
		if len(f.Types) > 0 && !containsString(f.Types, c.Type) {
			continue
		}
		if len(f.Modes) > 0 && !containsString(f.Modes, string(c.Mode)) {
			continue
		}
		if !topicsMatch(c.Topics, f.Topics) {
			continue
		}
		out = append(out, cloneContent(c))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Version < out[j].Version
	})
	return paginate(out, func(c domain.Content) string {
		// The keyset key carries the version so AllVersions paging stays
		// stable: two versions of one key never collide.
		return c.Key + "/" + pad(c.Version)
	}, p.Limit, p.Cursor, p.Order), nil
}

func (r *contentRepo) Versions(ctx context.Context, key string) ([]domain.ContentRef, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	r.guard()
	defer r.unguard()
	var out []domain.ContentRef
	for ref := range r.ds.contents {
		if ref.Key == key {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Tombstone hides every version of a key from default reads without deleting
// the bytes, which is what a content deletion must do so history survives.
func (r *contentRepo) Tombstone(ctx context.Context, key string, baseVersion int) error {
	if err := r.ready(); err != nil {
		return err
	}
	r.guard()
	defer r.unguard()
	head := r.headVersion(key)
	if head == 0 {
		return store.ErrNotFound
	}
	if baseVersion < 0 || baseVersion != head {
		return store.ErrConflict
	}
	for ref := range r.ds.contents {
		if ref.Key != key {
			continue
		}
		c := r.ds.contents[ref]
		c.Tombstoned = true
		r.ds.contents[ref] = c
	}
	return nil
}

func pad(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// --- roles ---

type roleRepo struct {
	st    *Store
	scope store.Scope
	ds    *datastore
	open  func() error
}

func (r *roleRepo) ready() error {
	if r.open != nil {
		return r.open()
	}
	return nil
}

// guard locks the store mutex unless this repo is transactional.
//
// A transactional repo runs inside Atomic, which already holds the store
// mutex for the whole callback; locking here would self-deadlock. A scoped
// repo runs unlocked, so it must take the mutex itself to keep the store's
// serializable guarantee.
func (r *roleRepo) guard() {
	if r.open == nil {
		r.st.mu.Lock()
	}
}

// unguard releases the lock taken by guard, if any.
func (r *roleRepo) unguard() {
	if r.open == nil {
		r.st.mu.Unlock()
	}
}

func (r *roleRepo) Put(ctx context.Context, role domain.Role, expectedRev int64) (domain.Role, error) {
	if err := r.ready(); err != nil {
		return domain.Role{}, err
	}
	r.guard()
	defer r.unguard()
	if role.ID == "" || !inScope(r.scope, role.Owner) {
		return domain.Role{}, store.ErrInvalid
	}
	cur, exists := r.ds.roles[role.ID]
	if !exists {
		if expectedRev != 0 {
			return domain.Role{}, store.ErrConflict
		}
		role.Rev = 1
		r.ds.roles[role.ID] = cloneRole(role)
		return cloneRole(role), nil
	}
	if cur.Rev != expectedRev {
		return domain.Role{}, store.ErrConflict
	}
	role.Rev = cur.Rev + 1
	r.ds.roles[role.ID] = cloneRole(role)
	return cloneRole(role), nil
}

func (r *roleRepo) Get(ctx context.Context, id string) (domain.Role, error) {
	if err := r.ready(); err != nil {
		return domain.Role{}, err
	}
	r.guard()
	defer r.unguard()
	role, ok := r.ds.roles[id]
	if !ok {
		return domain.Role{}, store.ErrNotFound
	}
	return cloneRole(role), nil
}

func (r *roleRepo) List(ctx context.Context, f store.RoleFilter, p store.PageRequest) (store.Page[domain.Role], error) {
	if err := r.ready(); err != nil {
		return store.Page[domain.Role]{}, err
	}
	r.guard()
	defer r.unguard()
	var out []domain.Role
	for _, role := range r.ds.roles {
		if !inScope(r.scope, role.Owner) {
			continue
		}
		if len(f.IDs) > 0 && !containsString(f.IDs, role.ID) {
			continue
		}
		if !topicsMatch(role.Topics, f.Topics) {
			continue
		}
		out = append(out, cloneRole(role))
	}
	return paginate(out, func(role domain.Role) string { return role.ID }, p.Limit, p.Cursor, p.Order), nil
}

func (r *roleRepo) Delete(ctx context.Context, id string, expectedRev int64) error {
	if err := r.ready(); err != nil {
		return err
	}
	r.guard()
	defer r.unguard()
	role, ok := r.ds.roles[id]
	if !ok {
		return store.ErrNotFound
	}
	if role.Rev != expectedRev {
		return store.ErrConflict
	}
	delete(r.ds.roles, id)
	return nil
}

// --- blobs ---

type blobRepo struct {
	st    *Store
	scope store.Scope
	ds    *datastore
	open  func() error
}

func (r *blobRepo) ready() error {
	if r.open != nil {
		return r.open()
	}
	return nil
}

// guard locks the store mutex unless this repo is transactional.
//
// A transactional repo runs inside Atomic, which already holds the store
// mutex for the whole callback; locking here would self-deadlock. A scoped
// repo runs unlocked, so it must take the mutex itself to keep the store's
// serializable guarantee.
func (r *blobRepo) guard() {
	if r.open == nil {
		r.st.mu.Lock()
	}
}

// unguard releases the lock taken by guard, if any.
func (r *blobRepo) unguard() {
	if r.open == nil {
		r.st.mu.Unlock()
	}
}

// Register records blob metadata, deduplicating on checksum. created reports
// whether a new record was written; a duplicate checksum returns the existing
// blob with created false.
func (r *blobRepo) Register(ctx context.Context, b domain.Blob) (domain.Blob, bool, error) {
	if err := r.ready(); err != nil {
		return domain.Blob{}, false, err
	}
	r.guard()
	defer r.unguard()
	if b.Checksum == "" || !inScope(r.scope, b.Owner) {
		return domain.Blob{}, false, store.ErrInvalid
	}
	for _, existing := range r.ds.blobs {
		if existing.Checksum == b.Checksum {
			return cloneBlob(existing), false, nil
		}
	}
	if b.ID == "" {
		return domain.Blob{}, false, store.ErrInvalid
	}
	if _, ok := r.ds.blobs[b.ID]; ok {
		return domain.Blob{}, false, store.ErrAlreadyExists
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = r.st.now()
	}
	if b.Remotes == nil {
		b.Remotes = map[string]domain.RemoteRef{}
	}
	r.ds.blobs[b.ID] = cloneBlob(b)
	r.ds.refs[b.ID] = nil
	return cloneBlob(b), true, nil
}

func (r *blobRepo) Get(ctx context.Context, id string) (domain.Blob, error) {
	if err := r.ready(); err != nil {
		return domain.Blob{}, err
	}
	r.guard()
	defer r.unguard()
	b, ok := r.ds.blobs[id]
	if !ok {
		return domain.Blob{}, store.ErrNotFound
	}
	return cloneBlob(b), nil
}

func (r *blobRepo) GetByChecksum(ctx context.Context, checksum string) (domain.Blob, error) {
	if err := r.ready(); err != nil {
		return domain.Blob{}, err
	}
	r.guard()
	defer r.unguard()
	for _, b := range r.ds.blobs {
		if b.Checksum == checksum {
			return cloneBlob(b), nil
		}
	}
	return domain.Blob{}, store.ErrNotFound
}

func (r *blobRepo) SetRemote(ctx context.Context, id string, provider string, remote domain.RemoteRef) error {
	if err := r.ready(); err != nil {
		return err
	}
	r.guard()
	defer r.unguard()
	b, ok := r.ds.blobs[id]
	if !ok {
		return store.ErrNotFound
	}
	if b.Remotes == nil {
		b.Remotes = map[string]domain.RemoteRef{}
	}
	b.Remotes[provider] = remote
	r.ds.blobs[id] = b
	return nil
}

func (r *blobRepo) AddRef(ctx context.Context, id string, holder domain.EntityRef) error {
	if err := r.ready(); err != nil {
		return err
	}
	r.guard()
	defer r.unguard()
	if _, ok := r.ds.blobs[id]; !ok {
		return store.ErrNotFound
	}
	for _, h := range r.ds.refs[id] {
		if h == holder {
			return nil
		}
	}
	r.ds.refs[id] = append(r.ds.refs[id], holder)
	return nil
}

func (r *blobRepo) RemoveRef(ctx context.Context, id string, holder domain.EntityRef) error {
	if err := r.ready(); err != nil {
		return err
	}
	r.guard()
	defer r.unguard()
	if _, ok := r.ds.blobs[id]; !ok {
		return store.ErrNotFound
	}
	cur := r.ds.refs[id]
	next := cur[:0]
	for _, h := range cur {
		if h != holder {
			next = append(next, h)
		}
	}
	r.ds.refs[id] = next
	return nil
}

func (r *blobRepo) Refs(ctx context.Context, id string) ([]domain.EntityRef, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	r.guard()
	defer r.unguard()
	if _, ok := r.ds.blobs[id]; !ok {
		return nil, store.ErrNotFound
	}
	return append([]domain.EntityRef(nil), r.ds.refs[id]...), nil
}

// Orphans lists blobs with no holder link that were created before the
// cutoff, which is what a garbage-collection pass deletes.
func (r *blobRepo) Orphans(ctx context.Context, olderThan time.Time, limit int) ([]domain.Blob, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	r.guard()
	defer r.unguard()
	var out []domain.Blob
	for id, b := range r.ds.blobs {
		if !inScope(r.scope, b.Owner) {
			continue
		}
		if len(r.ds.refs[id]) > 0 {
			continue
		}
		if !b.CreatedAt.Before(olderThan) {
			continue
		}
		out = append(out, cloneBlob(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *blobRepo) Delete(ctx context.Context, id string) error {
	if err := r.ready(); err != nil {
		return err
	}
	r.guard()
	defer r.unguard()
	if _, ok := r.ds.blobs[id]; !ok {
		return store.ErrNotFound
	}
	delete(r.ds.blobs, id)
	delete(r.ds.refs, id)
	return nil
}

// --- runs ---

type runRepo struct {
	st    *Store
	scope store.Scope
	ds    *datastore
	open  func() error
}

func (r *runRepo) ready() error {
	if r.open != nil {
		return r.open()
	}
	return nil
}

// guard locks the store mutex unless this repo is transactional.
//
// A transactional repo runs inside Atomic, which already holds the store
// mutex for the whole callback; locking here would self-deadlock. A scoped
// repo runs unlocked, so it must take the mutex itself to keep the store's
// serializable guarantee.
func (r *runRepo) guard() {
	if r.open == nil {
		r.st.mu.Lock()
	}
}

// unguard releases the lock taken by guard, if any.
func (r *runRepo) unguard() {
	if r.open == nil {
		r.st.mu.Unlock()
	}
}

func (r *runRepo) Create(ctx context.Context, run domain.Run) (domain.Run, error) {
	if err := r.ready(); err != nil {
		return domain.Run{}, err
	}
	r.guard()
	defer r.unguard()
	if run.ID == "" || run.ConversationID == "" {
		return domain.Run{}, store.ErrInvalid
	}
	if _, ok := r.ds.runs[run.ID]; ok {
		return domain.Run{}, store.ErrAlreadyExists
	}
	if run.Status == "" {
		run.Status = domain.RunQueued
	}
	now := r.st.now()
	if run.StartedAt.IsZero() {
		run.StartedAt = now
	}
	run.HeartbeatAt = now
	stored := cloneRun(run)
	r.ds.runs[run.ID] = stored
	return cloneRun(stored), nil
}

func (r *runRepo) Get(ctx context.Context, id string) (domain.Run, error) {
	if err := r.ready(); err != nil {
		return domain.Run{}, err
	}
	r.guard()
	defer r.unguard()
	run, ok := r.ds.runs[id]
	if !ok {
		return domain.Run{}, store.ErrNotFound
	}
	return cloneRun(run), nil
}

// Checkpoint advances the run's step with a compare-and-swap on the expected
// step, so two workers cannot both checkpoint the same step.
func (r *runRepo) Checkpoint(ctx context.Context, id string, expectedStep int, state json.RawMessage) (domain.Run, error) {
	if err := r.ready(); err != nil {
		return domain.Run{}, err
	}
	r.guard()
	defer r.unguard()
	run, ok := r.ds.runs[id]
	if !ok {
		return domain.Run{}, store.ErrNotFound
	}
	if run.Step != expectedStep {
		return domain.Run{}, store.ErrConflict
	}
	run.Step = expectedStep + 1
	if state != nil {
		run.State = append(json.RawMessage(nil), state...)
	}
	run.HeartbeatAt = r.st.now()
	r.ds.runs[id] = run
	return cloneRun(run), nil
}

// SetStatus moves a run between non-terminal states with the same
// compare-and-swap Checkpoint uses. A terminal status is rejected: once a run
// has finished, only a new run starts.
func (r *runRepo) SetStatus(ctx context.Context, id string, expectedStep int, status domain.RunStatus) (domain.Run, error) {
	if err := r.ready(); err != nil {
		return domain.Run{}, err
	}
	r.guard()
	defer r.unguard()
	run, ok := r.ds.runs[id]
	if !ok {
		return domain.Run{}, store.ErrNotFound
	}
	switch status {
	case domain.RunQueued, domain.RunRunning, domain.RunWaiting:
	default:
		return domain.Run{}, store.ErrInvalid
	}
	if run.Status.IsTerminal() {
		return domain.Run{}, store.ErrConflict
	}
	if run.Step != expectedStep {
		return domain.Run{}, store.ErrConflict
	}
	run.Status = status
	run.HeartbeatAt = r.st.now()
	r.ds.runs[id] = run
	return cloneRun(run), nil
}

func (r *runRepo) Heartbeat(ctx context.Context, id string, worker string) error {
	if err := r.ready(); err != nil {
		return err
	}
	r.guard()
	defer r.unguard()
	run, ok := r.ds.runs[id]
	if !ok {
		return store.ErrNotFound
	}
	run.Worker = worker
	run.HeartbeatAt = r.st.now()
	r.ds.runs[id] = run
	return nil
}

func (r *runRepo) Finish(ctx context.Context, id string, status domain.RunStatus, failure string) (domain.Run, error) {
	if err := r.ready(); err != nil {
		return domain.Run{}, err
	}
	r.guard()
	defer r.unguard()
	run, ok := r.ds.runs[id]
	if !ok {
		return domain.Run{}, store.ErrNotFound
	}
	switch status {
	case domain.RunSucceeded, domain.RunFailed, domain.RunCancelled:
	default:
		return domain.Run{}, store.ErrInvalid
	}
	run.Status = status
	run.Failure = failure
	run.FinishedAt = r.st.now()
	r.ds.runs[id] = run
	return cloneRun(run), nil
}

// Claim leases queued runs to a worker. A run whose heartbeat is older than
// staleBefore is considered abandoned and is reclaimable, which is the crash
// recovery path: a worker that dies mid-run stops heartbeating and another
// worker picks the run back up.
func (r *runRepo) Claim(ctx context.Context, staleBefore time.Time, limit int, worker string) ([]domain.Run, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	r.guard()
	defer r.unguard()
	if worker == "" || limit < 1 {
		return nil, store.ErrInvalid
	}
	now := r.st.now()

	// Collect the eligible ids before mutating: ranging over r.ds.runs while
	// writing into it is legal for an existing key, but reading the map into
	// a slice first keeps the mutation pass independent of the iteration.
	var eligible []string
	for id, run := range r.ds.runs {
		if run.Status == domain.RunQueued ||
			(run.Status == domain.RunRunning && run.HeartbeatAt.Before(staleBefore)) {
			eligible = append(eligible, id)
		}
	}
	sort.Strings(eligible)
	if len(eligible) > limit {
		eligible = eligible[:limit]
	}

	claimed := make([]domain.Run, 0, len(eligible))
	for _, id := range eligible {
		run := r.ds.runs[id]
		run.Status = domain.RunRunning
		run.Worker = worker
		run.HeartbeatAt = now
		r.ds.runs[id] = run
		claimed = append(claimed, cloneRun(run))
	}
	return claimed, nil
}

func (r *runRepo) ListActive(ctx context.Context, conversationID string) ([]domain.Run, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	r.guard()
	defer r.unguard()
	var out []domain.Run
	for _, run := range r.ds.runs {
		if run.ConversationID != conversationID {
			continue
		}
		if run.Status != domain.RunQueued && run.Status != domain.RunRunning {
			continue
		}
		out = append(out, cloneRun(run))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- receipts ---

type receiptRepo struct {
	st    *Store
	scope store.Scope
	ds    *datastore
	open  func() error
}

func (r *receiptRepo) ready() error {
	if r.open != nil {
		return r.open()
	}
	return nil
}

// guard locks the store mutex unless this repo is transactional.
//
// A transactional repo runs inside Atomic, which already holds the store
// mutex for the whole callback; locking here would self-deadlock. A scoped
// repo runs unlocked, so it must take the mutex itself to keep the store's
// serializable guarantee.
func (r *receiptRepo) guard() {
	if r.open == nil {
		r.st.mu.Lock()
	}
}

// unguard releases the lock taken by guard, if any.
func (r *receiptRepo) unguard() {
	if r.open == nil {
		r.st.mu.Unlock()
	}
}

// Reserve claims an idempotency key for ttl. The second call for the same
// key returns created false with the existing receipt, which is how a
// retried command is told "already in flight" without re-running it.
func (r *receiptRepo) Reserve(ctx context.Context, key string, ttl time.Duration) (domain.Receipt, bool, error) {
	if err := r.ready(); err != nil {
		return domain.Receipt{}, false, err
	}
	r.guard()
	defer r.unguard()
	if key == "" {
		return domain.Receipt{}, false, store.ErrInvalid
	}
	if existing, ok := r.ds.receipts[key]; ok {
		if r.st.now().After(existing.ExpiresAt) {
			delete(r.ds.receipts, key)
		} else {
			return cloneReceipt(existing), false, nil
		}
	}
	now := r.st.now()
	rec := domain.Receipt{
		Key:       key,
		State:     domain.ReceiptReserved,
		ExpiresAt: now.Add(ttl),
	}
	if ttl == 0 {
		rec.ExpiresAt = now.Add(defaultReceiptTTL)
	}
	r.ds.receipts[key] = cloneReceipt(rec)
	return cloneReceipt(rec), true, nil
}

const defaultReceiptTTL = 24 * time.Hour

func (r *receiptRepo) Complete(ctx context.Context, key string, result json.RawMessage) error {
	if err := r.ready(); err != nil {
		return err
	}
	r.guard()
	defer r.unguard()
	rec, ok := r.ds.receipts[key]
	if !ok {
		return store.ErrNotFound
	}
	if r.st.now().After(rec.ExpiresAt) {
		delete(r.ds.receipts, key)
		return store.ErrNotFound
	}
	rec.State = domain.ReceiptCompleted
	if result != nil {
		rec.Result = append(json.RawMessage(nil), result...)
	}
	r.ds.receipts[key] = rec
	return nil
}

// --- settings ---

type settingsRepo struct {
	st    *Store
	scope store.Scope
	ds    *datastore
	open  func() error
}

func (r *settingsRepo) ready() error {
	if r.open != nil {
		return r.open()
	}
	return nil
}

// guard locks the store mutex unless this repo is transactional.
//
// A transactional repo runs inside Atomic, which already holds the store
// mutex for the whole callback; locking here would self-deadlock. A scoped
// repo runs unlocked, so it must take the mutex itself to keep the store's
// serializable guarantee.
func (r *settingsRepo) guard() {
	if r.open == nil {
		r.st.mu.Lock()
	}
}

// unguard releases the lock taken by guard, if any.
func (r *settingsRepo) unguard() {
	if r.open == nil {
		r.st.mu.Unlock()
	}
}

func (r *settingsRepo) Get(ctx context.Context) (domain.Settings, error) {
	if err := r.ready(); err != nil {
		return domain.Settings{}, err
	}
	r.guard()
	defer r.unguard()
	key := settingsKey(r.scope)
	s, ok := r.ds.settings[key]
	if !ok {
		return domain.Settings{}, store.ErrNotFound
	}
	return cloneSettings(s), nil
}

func (r *settingsRepo) Put(ctx context.Context, s domain.Settings, expectedRev int64) (domain.Settings, error) {
	if err := r.ready(); err != nil {
		return domain.Settings{}, err
	}
	r.guard()
	defer r.unguard()
	key := settingsKey(r.scope)
	cur, exists := r.ds.settings[key]
	if !exists {
		if expectedRev != 0 {
			return domain.Settings{}, store.ErrConflict
		}
		s.Rev = 1
		if s.Owner == "" {
			s.Owner = r.scope.Owner
		}
		r.ds.settings[key] = cloneSettings(s)
		return cloneSettings(s), nil
	}
	if cur.Rev != expectedRev {
		return domain.Settings{}, store.ErrConflict
	}
	s.Rev = cur.Rev + 1
	r.ds.settings[key] = cloneSettings(s)
	return cloneSettings(s), nil
}

func settingsKey(scope store.Scope) string {
	if scope.System {
		return "system"
	}
	return scope.Owner
}

// --- maintenance ---

type maintenance struct {
	st    *Store
	scope store.Scope
}

func (m *maintenance) SweepTurns(ctx context.Context, opts store.SweepOptions) (store.SweepReport, error) {
	return store.SweepReport{}, nil
}

func (m *maintenance) SweepBlobs(ctx context.Context, opts store.SweepOptions) (store.SweepReport, error) {
	return store.SweepReport{}, nil
}

func (m *maintenance) SweepReceipts(ctx context.Context, now time.Time) (store.SweepReport, error) {
	return store.SweepReport{}, nil
}

// --- migrator ---

type migrator struct{}

func (migrator) Migrate(ctx context.Context) (store.MigrationReport, error) {
	return store.MigrationReport{From: schemaVersion, To: schemaVersion}, nil
}

func (migrator) SchemaVersion(ctx context.Context) (int, error) { return schemaVersion, nil }

const schemaVersion = 1
