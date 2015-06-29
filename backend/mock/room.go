package mock

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"euphoria.io/heim/backend"
	"euphoria.io/heim/proto"
	"euphoria.io/heim/proto/security"
	"euphoria.io/heim/proto/snowflake"
	"euphoria.io/scope"
)

type memRoom struct {
	m sync.Mutex

	name       string
	version    string
	log        *memLog
	agentBans  map[string]time.Time
	ipBans     map[string]time.Time
	identities map[string]proto.Identity
	live       map[string][]proto.Session

	sec        *proto.RoomSecurity
	messageKey *roomMessageKey
	managerKey *roomManagerKey
}

func NewRoom(
	ctx scope.Context, kms security.KMS, private bool, name, version string, managers ...proto.Account) (
	proto.Room, error) {

	sec, err := proto.NewRoomSecurity(kms, name)
	if err != nil {
		return nil, err
	}

	managerKey := sec.KeyEncryptingKey.Clone()
	if err := kms.DecryptKey(&managerKey); err != nil {
		return nil, err
	}
	roomKeyPair, err := sec.Unlock(&managerKey)
	if err != nil {
		return nil, err
	}

	room := &memRoom{
		name:      name,
		version:   version,
		log:       newMemLog(),
		agentBans: map[string]time.Time{},
		ipBans:    map[string]time.Time{},
		sec:       sec,
		managerKey: &roomManagerKey{
			RoomSecurity: sec,
			GrantManager: &proto.GrantManager{
				Capabilities:     &capabilities{},
				KeyEncryptingKey: &sec.KeyEncryptingKey,
				SubjectKeyPair:   &sec.KeyPair,
				SubjectNonce:     sec.Nonce,
			},
		},
	}
	room.managerKey.GrantManager.Managers = room.managerKey

	var (
		roomMsgKey proto.RoomMessageKey
		msgKey     security.ManagedKey
	)
	if private {
		roomMsgKey, err = room.GenerateMessageKey(ctx, kms)
		if err != nil {
			return nil, err
		}

		msgKey = roomMsgKey.ManagedKey()
		if err := kms.DecryptKey(&msgKey); err != nil {
			return nil, err
		}
	}

	for _, manager := range managers {
		kp := manager.KeyPair()
		c, err := security.GrantPublicKeyCapability(
			kms, sec.Nonce, roomKeyPair, &kp, nil, managerKey.Plaintext)
		if err != nil {
			return nil, err
		}
		room.managerKey.Capabilities.Save(ctx, manager, c)

		if private {
			c, err = security.GrantPublicKeyCapability(
				kms, roomMsgKey.Nonce(), roomKeyPair, &kp, nil, msgKey.Plaintext)
			if err != nil {
				return nil, err
			}
			room.messageKey.Capabilities.Save(ctx, manager, c)
		}
	}

	return room, nil
}

func (r *memRoom) Version() string { return r.version }

func (r *memRoom) GetMessage(ctx scope.Context, id snowflake.Snowflake) (*proto.Message, error) {
	return r.log.GetMessage(ctx, id)
}

func (r *memRoom) Latest(ctx scope.Context, n int, before snowflake.Snowflake) ([]proto.Message, error) {
	return r.log.Latest(ctx, n, before)
}

func (r *memRoom) Join(ctx scope.Context, session proto.Session) error {
	client := &proto.Client{}
	if !client.FromContext(ctx) {
		return fmt.Errorf("client data not found in scope")
	}

	r.m.Lock()
	defer r.m.Unlock()

	if r.identities == nil {
		r.identities = map[string]proto.Identity{}
	}
	if r.live == nil {
		r.live = map[string][]proto.Session{}
	}

	ident := session.Identity()
	id := ident.ID()

	if banned, ok := r.agentBans[client.Agent.IDString()]; ok && banned.After(time.Now()) {
		return proto.ErrAccessDenied
	}

	if banned, ok := r.ipBans[client.IP]; ok && banned.After(time.Now()) {
		return proto.ErrAccessDenied
	}

	if _, ok := r.identities[id]; !ok {
		r.identities[id] = ident
	}

	r.live[id] = append(r.live[id], session)
	return r.broadcast(ctx, proto.JoinType,
		proto.PresenceEvent(*session.View()), session)
}

func (r *memRoom) Part(ctx scope.Context, session proto.Session) error {
	r.m.Lock()
	defer r.m.Unlock()

	ident := session.Identity()
	id := ident.ID()
	live := r.live[id]
	for i, s := range live {
		if s == session {
			copy(live[i:], live[i+1:])
			r.live[id] = live[:len(live)-1]
		}
	}
	if len(r.live[id]) == 0 {
		delete(r.live, id)
		delete(r.identities, id)
	}
	return r.broadcast(ctx, proto.PartType,
		proto.PresenceEvent(*session.View()), session)
}

func (r *memRoom) Send(ctx scope.Context, session proto.Session, message proto.Message) (
	proto.Message, error) {

	r.m.Lock()
	defer r.m.Unlock()

	msg := proto.Message{
		ID:       message.ID,
		UnixTime: proto.Time(message.ID.Time()),
		Parent:   message.Parent,
		Sender:   message.Sender,
		Content:  message.Content,
	}
	r.log.post(&msg)
	return msg, r.broadcast(ctx, proto.SendType, msg, session)
}

func (r *memRoom) EditMessage(
	ctx scope.Context, session proto.Session, edit proto.EditMessageCommand) (
	proto.EditMessageReply, error) {

	editID, err := snowflake.New()
	if err != nil {
		return proto.EditMessageReply{}, err
	}

	msg, err := r.log.edit(edit)
	if err != nil {
		return proto.EditMessageReply{}, err
	}

	if edit.Announce {
		event := &proto.EditMessageEvent{
			EditID:  editID,
			Message: *msg,
		}
		if err := r.broadcast(ctx, proto.EditMessageType, event, session); err != nil {
			return proto.EditMessageReply{}, err
		}
	}

	reply := proto.EditMessageReply{
		EditID:  editID,
		Deleted: edit.Delete,
	}
	return reply, nil
}

func (r *memRoom) broadcast(
	ctx scope.Context, cmdType proto.PacketType, payload interface{}, excluding ...proto.Session) error {

	excMap := make(map[string]struct{}, len(excluding))
	for _, x := range excluding {
		if x != nil {
			excMap[x.ID()] = struct{}{}
		}
	}

	for _, sessions := range r.live {
		for _, session := range sessions {
			if _, ok := excMap[session.ID()]; ok {
				continue
			}
			if err := session.Send(ctx, cmdType.Event(), payload); err != nil {
				// TODO: accumulate errors
				return err
			}
		}
	}
	return nil
}

func (r *memRoom) Listing(ctx scope.Context) (proto.Listing, error) {
	listing := proto.Listing{}
	for _, sessions := range r.live {
		for _, session := range sessions {
			listing = append(listing, *session.View())
		}
	}
	sort.Sort(listing)
	return listing, nil
}

func (r *memRoom) RenameUser(
	ctx scope.Context, session proto.Session, formerName string) (*proto.NickEvent, error) {

	backend.Logger(ctx).Printf(
		"renaming %s from %s to %s\n", session.ID(), formerName, session.Identity().Name())
	payload := &proto.NickEvent{
		SessionID: session.ID(),
		ID:        session.Identity().ID(),
		From:      formerName,
		To:        session.Identity().Name(),
	}
	return payload, r.broadcast(ctx, proto.NickType, payload, session)
}

func (r *memRoom) MessageKey(ctx scope.Context) (proto.RoomMessageKey, error) {
	if r.messageKey == nil {
		return nil, nil
	}
	return r.messageKey, nil
}

func (r *memRoom) ManagerKey(ctx scope.Context) (proto.RoomManagerKey, error) {
	return r.managerKey, nil
}

func (r *memRoom) GenerateMessageKey(ctx scope.Context, kms security.KMS) (proto.RoomMessageKey, error) {
	nonce, err := kms.GenerateNonce(security.AES128.KeySize())
	if err != nil {
		return nil, err
	}

	mkey, err := kms.GenerateEncryptedKey(security.AES128, "room", r.name)
	if err != nil {
		return nil, err
	}

	kp := r.managerKey.KeyPair()
	r.messageKey = &roomMessageKey{
		GrantManager: &proto.GrantManager{
			Capabilities:     &capabilities{},
			Managers:         r.managerKey,
			KeyEncryptingKey: &r.sec.KeyEncryptingKey,
			SubjectKeyPair:   &kp,
			SubjectNonce:     nonce,
		},
		timestamp: time.Now(),
		nonce:     nonce,
		key:       *mkey,
	}
	r.messageKey.id = fmt.Sprintf("%s", r.messageKey.timestamp)
	return r.messageKey, nil
}

func (r *memRoom) BanAgent(ctx scope.Context, agentID string, until time.Time) error {
	r.m.Lock()
	r.agentBans[agentID] = until
	r.m.Unlock()
	return nil
}

func (r *memRoom) UnbanAgent(ctx scope.Context, agentID string) error {
	r.m.Lock()
	if _, ok := r.agentBans[agentID]; ok {
		delete(r.agentBans, agentID)
	}
	r.m.Unlock()
	return nil
}

func (r *memRoom) BanIP(ctx scope.Context, ip string, until time.Time) error {
	r.m.Lock()
	r.ipBans[ip] = until
	r.m.Unlock()
	return nil
}

func (r *memRoom) UnbanIP(ctx scope.Context, ip string) error {
	r.m.Lock()
	if _, ok := r.ipBans[ip]; ok {
		delete(r.ipBans, ip)
	}
	r.m.Unlock()
	return nil
}

func (r *memRoom) IsValidParent(id snowflake.Snowflake) (bool, error) {
	// TODO: actually check log to see if it is valid.
	return true, nil
}

func (r *memRoom) Managers(ctx scope.Context) ([]proto.Account, error) {
	caps := r.managerKey.Capabilities.(*capabilities)
	caps.Lock()
	defer caps.Unlock()

	managers := make([]proto.Account, 0, len(caps.accounts))
	for _, manager := range caps.accounts {
		managers = append(managers, manager)
	}
	return managers, nil
}

func (r *memRoom) verifyManager(ctx scope.Context, actor proto.Account, actorKey *security.ManagedKey) (
	*security.PublicKeyCapability, error) {

	// Verify that actorKey unlocks actor's keypair. In a real implementation,
	// we would take an additional step of verifying against a capability.
	kp := actor.KeyPair()
	if err := kp.Decrypt(actorKey); err != nil {
		return nil, err
	}

	// Verify actor is a manager.
	c, err := r.ManagerCapability(ctx, actor)
	if err != nil {
		if err == proto.ErrManagerNotFound {
			return nil, proto.ErrAccessDenied
		}
		return nil, err
	}

	return c.(*security.PublicKeyCapability), nil
}

func (r *memRoom) ManagerCapability(ctx scope.Context, manager proto.Account) (
	security.Capability, error) {

	c, err := r.managerKey.AccountCapability(ctx, manager)
	if err != nil {
		if err == proto.ErrAccessDenied {
			return nil, proto.ErrManagerNotFound
		}
		return nil, err
	}
	if c == nil {
		return nil, proto.ErrManagerNotFound
	}
	return c, nil
}

func (r *memRoom) AddManager(
	ctx scope.Context, kms security.KMS, actor proto.Account, actorKey *security.ManagedKey,
	newManager proto.Account) error {

	return r.managerKey.GrantToAccount(ctx, kms, actor, actorKey, newManager)
}

func (r *memRoom) RemoveManager(
	ctx scope.Context, actor proto.Account, actorKey *security.ManagedKey,
	formerManager proto.Account) error {

	if _, _, _, err := r.managerKey.Authority(ctx, actor, actorKey); err != nil {
		return err
	}

	if err := r.managerKey.RevokeFromAccount(ctx, formerManager); err != nil {
		if err == proto.ErrCapabilityNotFound || err == proto.ErrAccessDenied {
			return proto.ErrManagerNotFound
		}
		return err
	}
	return nil
}

func (r *memRoom) UpgradeRoom(ctx scope.Context, kms security.KMS) error { return nil }

type roomMessageKey struct {
	*proto.GrantManager
	id        string
	timestamp time.Time
	nonce     []byte
	key       security.ManagedKey
}

func (k *roomMessageKey) KeyID() string                   { return k.id }
func (k *roomMessageKey) Timestamp() time.Time            { return k.timestamp }
func (k *roomMessageKey) Nonce() []byte                   { return k.nonce }
func (k *roomMessageKey) ManagedKey() security.ManagedKey { return k.key.Clone() }

type roomManagerKey struct {
	*proto.GrantManager
	*proto.RoomSecurity
}

func (r *roomManagerKey) Nonce() []byte                    { return r.RoomSecurity.Nonce }
func (r *roomManagerKey) KeyPair() security.ManagedKeyPair { return r.RoomSecurity.KeyPair }
