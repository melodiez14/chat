package main

import (
	"log"
	"time"

	"github.com/abaron/chat/server/auth"
	"github.com/abaron/chat/server/push"
	"github.com/abaron/chat/server/store"
	"github.com/abaron/chat/server/store/types"
)

// Process request for a new account.
func replyCreateUser(s *Session, msg *ClientComMessage, rec *auth.Rec) {
	// The session cannot authenticate with the new account because  it's already authenticated.
	if msg.Acc.Login && (!s.uid.IsZero() || rec != nil) {
		s.queueOut(ErrAlreadyAuthenticated(msg.id, "", msg.timestamp))
		log.Println("create user: login requested while authenticated", s.sid)
		return
	}

	// Find authenticator for the requested scheme.
	authhdl := store.GetLogicalAuthHandler(msg.Acc.Scheme)
	if authhdl == nil {
		// New accounts must have an authentication scheme
		s.queueOut(ErrMalformed(msg.id, "", msg.timestamp))
		log.Println("create user: unknown auth handler", s.sid)
		return
	}

	// Check if login is unique.
	if ok, err := authhdl.IsUnique(msg.Acc.Secret); !ok {
		log.Println("create user: auth secret is not unique", err, s.sid)
		s.queueOut(decodeStoreError(err, msg.id, "", msg.timestamp,
			map[string]interface{}{"what": "auth"}))
		return
	}

	var user types.User
	var private interface{}

	// Ensure tags are unique and not restricted.
	if tags := normalizeTags(msg.Acc.Tags); tags != nil {
		if !restrictedTagsEqual(tags, nil, globals.immutableTagNS) {
			log.Println("create user: attempt to directly assign restricted tags", s.sid)
			msg := ErrPermissionDenied(msg.id, "", msg.timestamp)
			msg.Ctrl.Params = map[string]interface{}{"what": "tags"}
			s.queueOut(msg)
			return
		}
		user.Tags = tags
	}

	// Pre-check credentials for validity. We don't know user's access level
	// consequently cannot check presence of required credentials. Must do that later.
	creds := normalizeCredentials(msg.Acc.Cred, true)
	for i := range creds {
		cr := &creds[i]
		vld := store.GetValidator(cr.Method)
		if err := vld.PreCheck(cr.Value, cr.Params); err != nil {
			log.Println("create user: failed credential pre-check", cr, err, s.sid)
			s.queueOut(decodeStoreError(err, msg.Acc.Id, "", msg.timestamp,
				map[string]interface{}{"what": cr.Method}))
			return
		}
	}

	// Assign default access values in case the acc creator has not provided them
	user.Access.Auth = getDefaultAccess(types.TopicCatP2P, true) | getDefaultAccess(types.TopicCatGrp, true)
	user.Access.Anon = getDefaultAccess(types.TopicCatP2P, false) | getDefaultAccess(types.TopicCatGrp, false)

	// Assign actual access values, public and private.
	if msg.Acc.Desc != nil {
		if msg.Acc.Desc.DefaultAcs != nil {
			if msg.Acc.Desc.DefaultAcs.Auth != "" {
				user.Access.Auth.UnmarshalText([]byte(msg.Acc.Desc.DefaultAcs.Auth))
				user.Access.Auth &= types.ModeCP2P
				if user.Access.Auth != types.ModeNone {
					user.Access.Auth |= types.ModeApprove
				}
			}
			if msg.Acc.Desc.DefaultAcs.Anon != "" {
				user.Access.Anon.UnmarshalText([]byte(msg.Acc.Desc.DefaultAcs.Anon))
				user.Access.Anon &= types.ModeCP2P
				if user.Access.Anon != types.ModeNone {
					user.Access.Anon |= types.ModeApprove
				}
			}
		}
		if !isNullValue(msg.Acc.Desc.Public) {
			user.Public = msg.Acc.Desc.Public
		}
		if !isNullValue(msg.Acc.Desc.Private) {
			private = msg.Acc.Desc.Private
		}
	}

	// Create user record in the database.
	if _, err := store.Users.Create(&user, private); err != nil {
		log.Println("create user: failed to create user", err, s.sid)
		s.queueOut(ErrUnknown(msg.id, "", msg.timestamp))
		return
	}

	// Add authentication record. The authhdl.AddRecord may change tags.
	rec, err := authhdl.AddRecord(&auth.Rec{Uid: user.Uid(), Tags: user.Tags}, msg.Acc.Secret)
	if err != nil {
		log.Println("create user: add auth record failed woi", err, s.sid)
		// Attempt to delete incomplete user record
		store.Users.Delete(user.Uid(), false)
		s.queueOut(decodeStoreError(err, msg.id, "", msg.timestamp, nil))
		return
	}

	// When creating an account, the user must provide all required credentials.
	// If any are missing, reject the request.
	if len(creds) < len(globals.authValidators[rec.AuthLevel]) {
		log.Println("create user: missing credentials; have:", creds, "want:", globals.authValidators[rec.AuthLevel], s.sid)
		// Attempt to delete incomplete user record
		store.Users.Delete(user.Uid(), false)
		_, missing := stringSliceDelta(globals.authValidators[rec.AuthLevel], credentialMethods(creds))
		s.queueOut(decodeStoreError(types.ErrPolicy, msg.id, "", msg.timestamp,
			map[string]interface{}{"creds": missing}))
		return
	}

	// Save credentials, update tags if necessary.
	tmpToken, _, _ := store.GetLogicalAuthHandler("token").GenSecret(&auth.Rec{
		Uid:       user.Uid(),
		AuthLevel: auth.LevelNone,
		Lifetime:  time.Hour * 24,
		Features:  auth.FeatureNoLogin})
	validated, _, err := addCreds(user.Uid(), creds, rec.Tags, s.lang, tmpToken)
	if err != nil {
		// Delete incomplete user record.
		store.Users.Delete(user.Uid(), false)
		log.Println("create user: failed to save or validate credential", err, s.sid)
		s.queueOut(decodeStoreError(err, msg.id, "", msg.timestamp, nil))
		return
	}

	var reply *ServerComMessage
	if msg.Acc.Login {
		// Process user's login request.
		_, missing := stringSliceDelta(globals.authValidators[rec.AuthLevel], validated)
		reply = s.onLogin(msg.id, msg.timestamp, rec, missing)
	} else {
		// Not using the new account for logging in.
		reply = NoErrCreated(msg.id, "", msg.timestamp)
		reply.Ctrl.Params = map[string]interface{}{
			"user":    user.Uid().UserId(),
			"authlvl": rec.AuthLevel.String(),
		}
	}
	params := reply.Ctrl.Params.(map[string]interface{})
	params["desc"] = &MsgTopicDesc{
		CreatedAt: &user.CreatedAt,
		UpdatedAt: &user.UpdatedAt,
		DefaultAcs: &MsgDefaultAcsMode{
			Auth: user.Access.Auth.String(),
			Anon: user.Access.Anon.String()},
		Public:  user.Public,
		Private: private}

	s.queueOut(reply)

	pluginAccount(&user, plgActCreate)
}

// Process update to an account:
// * Authentication update, i.e. login/password change
// * Credentials update
func replyUpdateUser(s *Session, msg *ClientComMessage, rec *auth.Rec) {
	if s.uid.IsZero() && rec == nil {
		// Session is not authenticated and no token provided.
		log.Println("replyUpdateUser: not a new account and not authenticated", s.sid)
		s.queueOut(ErrPermissionDenied(msg.id, "", msg.timestamp))
		return
	} else if msg.from != "" && rec != nil {
		// Two UIDs: one from msg.from, one from token. Ambigous, reject.
		log.Println("replyUpdateUser: got both authenticated session and token", s.sid)
		s.queueOut(ErrMalformed(msg.id, "", msg.timestamp))
		return
	}

	userId := msg.from
	authLvl := auth.Level(msg.authLvl)
	if rec != nil {
		userId = rec.Uid.UserId()
		authLvl = rec.AuthLevel
	}

	if msg.Acc.User != "" && msg.Acc.User != userId {
		if s.authLvl != auth.LevelRoot {
			log.Println("replyUpdateUser: attempt to change another's account by non-root", s.sid)
			s.queueOut(ErrPermissionDenied(msg.id, "", msg.timestamp))
			return
		}
		// Root is editing someone else's account.
		userId = msg.Acc.User
		authLvl = auth.ParseAuthLevel(msg.Acc.AuthLevel)
	}

	uid := types.ParseUserId(userId)
	if uid.IsZero() || authLvl == auth.LevelNone {
		// Either msg.Acc.User or msg.Acc.AuthLevel contains invalid data.
		s.queueOut(ErrMalformed(msg.id, "", msg.timestamp))
		log.Println("replyUpdateUser: either user id or auth level is missing", s.sid)
		return
	}

	user, err := store.Users.Get(uid)
	if user == nil && err == nil {
		err = types.ErrNotFound
	}
	if err != nil {
		log.Println("replyUpdateUser: failed to fetch user from DB", err, s.sid)
		s.queueOut(decodeStoreError(err, msg.id, "", msg.timestamp, nil))
		return
	}

	var params map[string]interface{}
	if msg.Acc.Scheme != "" {
		err = updateUserAuth(msg, user, rec)
	} else if len(msg.Acc.Cred) > 0 {
		// Handle request to update credentials.
		tmpToken, _, _ := store.GetLogicalAuthHandler("token").GenSecret(&auth.Rec{
			Uid:       uid,
			AuthLevel: auth.LevelNone,
			Lifetime:  time.Hour * 24,
			Features:  auth.FeatureNoLogin})
		_, _, err := addCreds(uid, msg.Acc.Cred, nil, s.lang, tmpToken)
		if err == nil {
			if allCreds, err := store.Users.GetAllCreds(uid, "", true); err != nil {
				var validated []string
				for i := range allCreds {
					validated = append(validated, allCreds[i].Method)
				}
				_, missing := stringSliceDelta(globals.authValidators[authLvl], validated)
				if len(missing) > 0 {
					params = map[string]interface{}{"cred": missing}
				}
			}
		}
	} else {
		err = types.ErrMalformed
	}

	if err != nil {
		log.Println("replyUpdateUser: failed to update user", err, s.sid)
		s.queueOut(decodeStoreError(err, msg.id, "", msg.timestamp, nil))
		return
	}

	s.queueOut(NoErrParams(msg.id, "", msg.timestamp, params))

	// Call plugin with the account update
	pluginAccount(user, plgActUpd)
}

// Authentication update
func updateUserAuth(msg *ClientComMessage, user *types.User, rec *auth.Rec) error {
	authhdl := store.GetLogicalAuthHandler(msg.Acc.Scheme)
	if authhdl != nil {
		// Request to update auth of an existing account. Only basic & rest auth are currently supported

		// TODO(gene): support adding new auth schemes

		rec, err := authhdl.UpdateRecord(&auth.Rec{Uid: user.Uid(), Tags: user.Tags}, msg.Acc.Secret)
		if err != nil {
			return err
		}

		// Tags may have been changed by authhdl.UpdateRecord, reset them.
		// Can't do much with the error here, so ignoring it.
		store.Users.UpdateTags(user.Uid(), nil, nil, rec.Tags)
		return nil
	}

	// Invalid or unknown auth scheme
	return types.ErrMalformed
}

// addCreds adds new credentials and re-send validation request for existing ones. It also adds credential-defined
// tags if necessary.
// Returns methods validated in this call only, full set of tags.
func addCreds(uid types.Uid, creds []MsgCredClient, tags []string, lang string,
	tmpToken []byte) ([]string, []string, error) {
	var validated []string
	for i := range creds {
		cr := &creds[i]
		vld := store.GetValidator(cr.Method)
		if vld == nil {
			// Ignore unknown validator.
			continue
		}

		isNew, err := vld.Request(uid, cr.Value, lang, cr.Response, tmpToken)
		if err != nil {
			return nil, nil, err
		}

		if isNew && cr.Response != "" {
			// If response is provided and vld.Request did not return an error, the new request was
			// successfully validated.
			validated = append(validated, cr.Method)

			// Generate tags for these confirmed credentials.
			if globals.validators[cr.Method].addToTags {
				tags = append(tags, cr.Method+":"+cr.Value)
			}
		}
	}

	// Save tags potentially changed by the validator.
	if len(tags) > 0 {
		tags, _ = store.Users.UpdateTags(uid, tags, nil, nil)
	}
	return validated, tags, nil
}

// validatedCreds returns the list of validated credentials including those validated in this call.
// Returns all validated methods including those validated earlier and now, full set of tags.
func validatedCreds(uid types.Uid, authLvl auth.Level, creds []MsgCredClient, errorOnFail bool) ([]string, []string, error) {

	// Check if credential validation is required.
	if len(globals.authValidators[authLvl]) == 0 {
		return nil, nil, nil
	}

	// Get all validated methods
	allCreds, err := store.Users.GetAllCreds(uid, "", true)
	if err != nil {
		return nil, nil, err
	}

	methods := make(map[string]struct{})
	for i := range allCreds {
		methods[allCreds[i].Method] = struct{}{}
	}

	// Add credentials which are validated in this call.
	// Unknown validators are removed.
	creds = normalizeCredentials(creds, false)
	var tagsToAdd []string
	for i := range creds {
		cr := &creds[i]
		if cr.Response == "" {
			// Ignore empty response.
			continue
		}

		vld := store.GetValidator(cr.Method) // No need to check for nil, unknown methods are removed earlier.
		value, err := vld.Check(uid, cr.Response)
		if err != nil {
			// Check failed.
			if storeErr, ok := err.(types.StoreError); ok && storeErr == types.ErrCredentials {
				if errorOnFail {
					// Report invalid response.
					return nil, nil, types.ErrInvalidResponse
				}
				// Skip invalid response. Keep credential unvalidated.
				continue
			}
			// Actual error. Report back.
			return nil, nil, err
		}

		// Check did not return an error: the request was successfully validated.
		methods[cr.Method] = struct{}{}

		// Add validated credential to user's tags.
		if globals.validators[cr.Method].addToTags {
			tagsToAdd = append(tagsToAdd, cr.Method+":"+value)
		}
	}

	var tags []string
	if len(tagsToAdd) > 0 {
		// Save update to tags
		tags, _ = store.Users.UpdateTags(uid, tagsToAdd, nil, nil)
	}

	var validated []string
	for method := range methods {
		validated = append(validated, method)
	}

	return validated, tags, nil
}

// deleteCred deletes user's credential.
func deleteCred(uid types.Uid, authLvl auth.Level, cred *MsgCredClient) ([]string, error) {
	vld := store.GetValidator(cred.Method)
	if vld == nil || cred.Value == "" {
		// Reject invalid request: unknown validation method or missing credential value.
		return nil, types.ErrMalformed
	}

	// Is this a required credential for this validation level?
	var isRequired bool
	for _, method := range globals.authValidators[authLvl] {
		if method == cred.Method {
			isRequired = true
			break
		}
	}

	// If credential is required, make sure the method remains validated even after this credential is deleted.
	if isRequired {

		// There could be multiple validated credentials for the same method thus we are getting a map with count
		// for each method.

		// Get all credentials of the given method.
		allCreds, err := store.Users.GetAllCreds(uid, cred.Method, false)
		if err != nil {
			return nil, err
		}

		// Check if it's OK to delete: there is another validated value.
		var okTodelete bool
		for _, cr := range allCreds {
			if cr.Done && cr.Value != cred.Value {
				okTodelete = true
				break
			}
		}

		if !okTodelete {
			// Reject: this is the only validated credential and it must be provided.
			return nil, types.ErrPolicy
		}
	}

	// The credential is either not required or more than one credential is validated for the given method.
	err := vld.Remove(uid, cred.Value)
	if err != nil {
		return nil, err
	}

	// Remove generated tags for the deleted credential.
	var tags []string
	if globals.validators[cred.Method].addToTags {
		// This error should not be returned to user.
		tags, _ = store.Users.UpdateTags(uid, nil, []string{cred.Method + ":" + cred.Value}, nil)
	}
	return tags, nil
}

// Request to delete a user:
// 1. Disable user's login
// 2. Terminate all user's sessions except the current session.
// 3. Stop all active topics
// 4. Notify other subscribers that topics are being deleted.
// 5. Delete user from the database.
// 6. Report success or failure.
// 7. Terminate user's last session.
func replyDelUser(s *Session, msg *ClientComMessage) {
	var reply *ServerComMessage
	var uid types.Uid

	if msg.Del.User == "" || msg.Del.User == s.uid.UserId() {
		// Delete current user.
		uid = s.uid
	} else if s.authLvl == auth.LevelRoot {
		// Delete another user.
		uid = types.ParseUserId(msg.Del.User)
		if uid.IsZero() {
			reply = ErrMalformed(msg.id, "", msg.timestamp)
			log.Println("replyDelUser: invalid user ID", msg.Del.User, s.sid)
		}
	} else {
		reply = ErrPermissionDenied(msg.id, "", msg.timestamp)
		log.Println("replyDelUser: illegal attempt to delete another user", msg.Del.User, s.sid)
	}

	if reply == nil {
		// Disable all authenticators
		authnames := store.GetAuthNames()
		for _, name := range authnames {
			if err := store.GetAuthHandler(name).DelRecords(uid); err != nil {
				// This could be completely benign, i.e. authenticator exists but not used.
				log.Println("replyDelUser: failed to delete auth record", uid.UserId(), name, err, s.sid)
			}
		}

		// Terminate all sessions. Skip the current session so the requester gets a response.
		globals.sessionStore.EvictUser(uid, s.sid)

		// Stop topics where the user is the owner and p2p topics.
		done := make(chan bool)
		globals.hub.unreg <- &topicUnreg{forUser: uid, del: msg.Del.Hard, done: done}
		<-done

		// Notify users of interest that the user is gone.
		if uoi, err := store.Users.GetSubs(uid, nil); err == nil {
			presUsersOfInterestOffline(uid, uoi, "gone")
		} else {
			log.Println("replyDelUser: failed to send notifications to users", err, s.sid)
		}

		// Notify subscribers of the group topics where the user was the owner that the topics were deleted.
		if ownTopics, err := store.Users.GetOwnTopics(uid, nil); err == nil {
			for _, topicName := range ownTopics {
				if subs, err := store.Topics.GetSubs(topicName, nil); err == nil {
					presSubsOfflineOffline(topicName, types.TopicCatGrp, subs, "gone", &presParams{}, s.sid)
				}
			}
		} else {
			log.Println("replyDelUser: failed to send notifications to owned topics", err, s.sid)
		}

		// Delete user's records from the database.
		if err := store.Users.Delete(uid, msg.Del.Hard); err != nil {
			reply = decodeStoreError(err, msg.id, "", msg.timestamp, nil)
			log.Println("replyDelUser: failed to delete user", err, s.sid)
		} else {
			reply = NoErr(msg.id, "", msg.timestamp)
		}
	}

	s.queueOut(reply)

	if s.uid == uid {
		// Evict the current session if it belongs to the deleted user.
		s.stop <- s.serialize(NoErrEvicted("", "", msg.timestamp))
	}
}

type userUpdate struct {
	// Single user ID being updated
	uid types.Uid
	// User IDs being updated
	uidList []types.Uid
	// Unread count
	unread int
	// Treat the count as an increment as opposite to the final value.
	inc bool

	// Optional push notification
	pushRcpt *push.Receipt
}

type userCacheEntry struct {
	unread int
	topics int
}

var usersCache map[types.Uid]userCacheEntry

// Initialize users cache.
func usersInit() {
	usersCache = make(map[types.Uid]userCacheEntry)

	globals.usersUpdate = make(chan *userUpdate, 1024)

	go userUpdater()
}

// Shutdown users cache.
func usersShutdown() {
	if globals.statsUpdate != nil {
		globals.statsUpdate <- nil
	}
}

func usersUpdateUnread(uid types.Uid, val int, inc bool) {
	if globals.usersUpdate != nil && (!inc || val != 0) {
		select {
		case globals.usersUpdate <- &userUpdate{uid: uid, unread: val, inc: inc}:
		default:
		}
	}
}

// Process push notification.
func usersPush(rcpt *push.Receipt) {
	if globals.usersUpdate != nil {
		select {
		case globals.usersUpdate <- &userUpdate{pushRcpt: rcpt}:
		default:
		}
	}
}

// Start tracking a single user. Used for cache management.
func usersRegisterUser(uid types.Uid, add bool) {
	if globals.usersUpdate == nil {
		return
	}

	upd := &userUpdate{uidList: make([]types.Uid, 1), inc: add}
	upd.uidList[0] = uid

	select {
	case globals.usersUpdate <- upd:
	default:
	}

}

// Account users as members of an active topic. Used for cache management.
func usersRegisterTopic(t *Topic, add bool) {
	if globals.usersUpdate == nil {
		return
	}

	if len(t.perUser) == 0 {
		// me and fnd topics
		return
	}

	upd := &userUpdate{uidList: make([]types.Uid, len(t.perUser)), inc: add}
	i := 0
	for uid := range t.perUser {
		upd.uidList[i] = uid
		i++
	}

	select {
	case globals.usersUpdate <- upd:
	default:
	}
}

// The go routine for processing updates to users cache.
func userUpdater() {
	unreadUpdater := func(uid types.Uid, val int, inc bool) int {
		uce, ok := usersCache[uid]
		if !ok {
			log.Println("ERROR: attempt to update unread count for user who has not been loaded")
			return -1
		}

		if uce.unread < 0 {
			count, err := store.Users.GetUnreadCount(uid)
			if err != nil {
				log.Println("users: failed to load unread count", err)
				return -1
			}
			uce.unread = count
		} else if inc {
			uce.unread += val
		} else {
			uce.unread = val
		}

		usersCache[uid] = uce

		return uce.unread
	}

	for upd := range globals.usersUpdate {
		// Shutdown requested.
		if upd == nil {
			globals.usersUpdate = nil
			// Dont' care to close the channel.
			break
		}

		// Request to send push notifications.
		if upd.pushRcpt != nil {
			for uid, rcptTo := range upd.pushRcpt.To {
				// Handle update
				unread := unreadUpdater(uid, 1, true)
				if unread >= 0 {
					rcptTo.Unread = unread
					upd.pushRcpt.To[uid] = rcptTo
				}
			}
			push.Push(upd.pushRcpt)
			continue
		}

		// Request to add/remove user from cache.
		if len(upd.uidList) > 0 {
			for _, uid := range upd.uidList {
				uce, ok := usersCache[uid]
				if upd.inc {
					if !ok {
						// This is a registration of a new user.
						// We are not loading unread count here, so set it to -1.
						uce.unread = -1
					}
					uce.topics++
					usersCache[uid] = uce
				} else if ok {
					if uce.topics > 1 {
						uce.topics--
						usersCache[uid] = uce
					} else {
						// Remove user from cache
						delete(usersCache, uid)
					}
				} else {
					// BUG!
					log.Println("ERROR: request to unregister user which has not been registered")
				}
			}

			continue
		}

		// Request to update unread count.
		unreadUpdater(upd.uid, upd.unread, upd.inc)
	}

	log.Println("users: shutdown")
}
