package wire

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"hash"
	"strconv"
	"strings"

	"golang.org/x/crypto/pbkdf2"

	"mlstoredb/db"
)

const scramIterations = 15000

type scramState struct {
	username        string
	clientFirstBare string
	serverFirst     string
	saltedPassword  []byte
	skipEmpty       bool
	step            int
}

func binaryPayload(v any) ([]byte, bool) {
	if v == nil {
		return nil, true
	}
	if doc, ok := v.(db.Document); ok {
		if b, ok := doc["$binary"]; ok {
			raw, _, err := parseBinaryExtended(b)
			if err != nil {
				return nil, false
			}
			return raw, true
		}
		return nil, false
	}
	return nil, false
}

func binaryDoc(raw []byte) db.Document {
	return db.Document{"$binary": db.Document{
		"base64":  base64.StdEncoding.EncodeToString(raw),
		"subType": "00",
	}}
}

func (s *Server) cmdSaslStart(ctx *connCtx, arg any, cmd db.Document) db.Document {
	if s.opts.AuthUser == "" {
		// El servidor no tiene credenciales de wire: cualquier intento de login falla aquí.
		// Los clientes Mongo muestran este errmsg como "invalid credentials"/"Authentication
		// failed", así que se explica la causa y se deja constancia en la consola del servidor.
		s.logf("wire: saslStart rejected from %s: server started without -user/-pass "+
			"(wire auth disabled) - no client can authenticate", ctx.remote)
		return cmdErrorReply(errWire(18, "AuthenticationFailed",
			"Authentication failed: this server has no wire credentials. "+
				"Start mls-server with -user/-pass (and a user that exists in the database) to enable SCRAM-SHA-256"))
	}
	mech, _ := cmd["mechanism"].(string)
	if mech != "SCRAM-SHA-256" {
		s.logf("wire: unsupported authentication mechanism %q from %s", mech, ctx.remote)
		return cmdErrorReply(errWire(2, "BadValue", "unsupported mechanism: "+mech))
	}
	payload, ok := binaryPayload(cmd["payload"])
	if !ok {
		return cmdErrorReply(errWire(9, "FailedToParse", "saslStart payload must be binary"))
	}
	msg := string(payload)
	if !strings.HasPrefix(msg, "n,") && !strings.HasPrefix(msg, "y,") {
		return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
	}
	idx := strings.Index(msg, ",,")
	if idx < 0 {
		return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
	}
	bare := msg[idx+2:]
	username, cnonce, err := parseScramAttr(bare, "n", "r")
	if err != nil {
		return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
	}
	if username != s.opts.AuthUser {
		s.logf("wire: login rejected from %s: client asked for %q but the wire only accepts %q "+
			"(-user); use that user or restart with -user %s", ctx.remote, username, s.opts.AuthUser, username)
		return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
	}
	var saltRaw [16]byte
	if _, err := rand.Read(saltRaw[:]); err != nil {
		return cmdErrorReply(errWire(1, "InternalError", "salt generation failed"))
	}
	var snonceRaw [18]byte
	if _, err := rand.Read(snonceRaw[:]); err != nil {
		return cmdErrorReply(errWire(1, "InternalError", "nonce generation failed"))
	}
	snonce := base64.RawStdEncoding.EncodeToString(snonceRaw[:])
	salt := base64.StdEncoding.EncodeToString(saltRaw[:])
	serverFirst := "r=" + cnonce + snonce + ",s=" + salt + ",i=" + strconv.Itoa(scramIterations)
	salted := pbkdf2.Key([]byte(s.opts.AuthPass), saltRaw[:], scramIterations, 32, sha256.New)
	skipEmpty := false
	if opts, ok := cmd["options"].(db.Document); ok {
		if se, ok := opts["skipEmptyExchange"].(bool); ok {
			skipEmpty = se
		}
	}
	ctx.scram = &scramState{
		username:        username,
		clientFirstBare: bare,
		serverFirst:     serverFirst,
		saltedPassword:  salted,
		skipEmpty:       skipEmpty,
		step:            1,
	}
	return okReply(
		"conversationId", int32(1),
		"done", false,
		"payload", binaryDoc([]byte(serverFirst)),
	)
}

func (s *Server) cmdSaslContinue(ctx *connCtx, arg any, cmd db.Document) db.Document {
	st := ctx.scram
	if st == nil {
		s.logf("wire: saslContinue without a previous saslStart from %s", ctx.remote)
		return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
	}
	payload, ok := binaryPayload(cmd["payload"])
	if !ok {
		return cmdErrorReply(errWire(9, "FailedToParse", "saslContinue payload must be binary"))
	}
	if st.step == 2 {
		ctx.scram = nil
		return okReply("conversationId", int32(1), "done", true, "payload", binaryDoc(nil))
	}
	msg := string(payload)
	parts := strings.Split(msg, ",")
	var c, r, p string
	for _, part := range parts {
		if strings.HasPrefix(part, "c=") {
			c = part[2:]
		} else if strings.HasPrefix(part, "r=") {
			r = part[2:]
		} else if strings.HasPrefix(part, "p=") {
			p = part[2:]
		}
	}
	if c == "" || r == "" || p == "" {
		return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
	}
	if !strings.HasSuffix(st.serverFirst, ",s=") {
		expectedR := st.serverFirst
		if i := strings.Index(expectedR, ",s="); i >= 0 {
			expectedR = expectedR[:i]
		}
		if "r="+r != expectedR {
			return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
		}
	}
	proof, err := base64.StdEncoding.DecodeString(p)
	if err != nil {
		return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
	}
	withoutProof := strings.Join(parts[:len(parts)-1], ",")
	authMessage := st.clientFirstBare + "," + st.serverFirst + "," + withoutProof
	clientKey := hmacSHA256(st.saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	clientSignature := hmacSHA256(storedKey[:], []byte(authMessage))
	if len(proof) != len(clientSignature) {
		return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
	}
	recovered := make([]byte, len(proof))
	for i := range proof {
		recovered[i] = proof[i] ^ clientSignature[i]
	}
	recHash := sha256.Sum256(recovered)
	if !hmac.Equal(recHash[:], storedKey[:]) {
		// Prueba SCRAM incorrecta = la contraseña enviada por el cliente no coincide con -pass.
		s.logf("wire: wrong password for %q from %s (does not match -pass)", st.username, ctx.remote)
		return cmdErrorReply(errWire(18, "AuthenticationFailed", "Authentication failed."))
	}
	serverKey := hmacSHA256(st.saltedPassword, []byte("Server Key"))
	serverSignature := hmacSHA256(serverKey, []byte(authMessage))
	ctx.authed = true
	if s.store.AuthActive() {
		sess, err := s.store.Authenticate(s.opts.AuthUser, s.opts.AuthPass)
		if err != nil {
			ctx.authed = false
			ctx.scram = nil
			// La base tiene RBAC activo: el gate del wire y los usuarios del motor (_users)
			// son listas distintas, así que -user/-pass debe coincidir con un usuario real.
			s.logf("wire: SCRAM proof for %q matches -pass but the engine rejected it: %v. "+
				"This database has RBAC active: -user/-pass must be an existing user in _users "+
				"(web console -> Users)", s.opts.AuthUser, err)
			return cmdErrorReply(errWire(18, "AuthenticationFailed",
				"Authentication failed: this database has users (RBAC active) and the wire user is not one of them. "+
					"Use -user/-pass with an existing database user"))
		}
		ctx.session = sess
		s.logf("wire: %q authenticated from %s (engine session, roles=%v)", s.opts.AuthUser, ctx.remote, sess.Roles())
	} else {
		s.logf("wire: %q authenticated from %s (database has no users: RBAC inactive)", s.opts.AuthUser, ctx.remote)
	}
	if st.skipEmpty {
		ctx.scram = nil
		return okReply(
			"conversationId", int32(1),
			"done", true,
			"payload", binaryDoc([]byte("v="+base64.StdEncoding.EncodeToString(serverSignature))),
		)
	}
	st.step = 2
	return okReply(
		"conversationId", int32(1),
		"done", false,
		"payload", binaryDoc([]byte("v="+base64.StdEncoding.EncodeToString(serverSignature))),
	)
}

func parseScramAttr(msg string, nameKeys ...string) (string, string, error) {
	var vals []string
	for _, k := range nameKeys {
		found := ""
		for _, part := range strings.Split(msg, ",") {
			if strings.HasPrefix(part, k+"=") {
				found = part[2:]
				break
			}
		}
		if found == "" {
			return "", "", errors.New("missing attribute " + k)
		}
		vals = append(vals, found)
	}
	return vals[0], vals[1], nil
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(func() hash.Hash { return sha256.New() }, key)
	h.Write(data)
	return h.Sum(nil)
}
