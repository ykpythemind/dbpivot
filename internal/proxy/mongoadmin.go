package proxy

import "strings"

// Local admin-command responder for the pre-routing handshake chatter.
//
// dbpivot defers MongoDB routing until a command's $db names a configured
// virtual database (see dispatchMongo in mongoserver.go). But real drivers and
// mongosh do not jump straight from the hello handshake to a user command —
// they first issue a burst of administrative commands on $db:admin to probe the
// server: ping, buildInfo, getParameter (featureCompatibilityVersion), getLog
// (startupWarnings), whatsmyuri, connectionStatus, getCmdLineOpts, endSessions.
// Those commands name no configured database, so the proxy has no upstream to
// forward them to yet. Before this responder, dispatchMongo answered them with
// an ok:0 InvalidNamespace error, which made mongosh and the official drivers
// abort the connection during connect.
//
// BuildAdminCommandReply answers the harmless, fixed-shape subset of those
// commands locally — enough for a client to finish connecting — and reports
// whether it recognised the command. Anything it does not recognise (a real
// admin operation, an Atlas-only probe, ...) is left to the caller's normal
// path, which still returns a command error; drivers tolerate that for the
// optional probes. The replies describe the proxy itself (an auth-disabled
// standalone), consistent with BuildHelloReply; they are never forwarded
// upstream.

// Version the proxy reports for buildInfo / featureCompatibilityVersion. It is
// kept consistent with mongoMaxWireVersion (17 == MongoDB 6.0) so a driver that
// gates features on the server version agrees with the wire level advertised in
// the hello reply.
const (
	mongoFakeVersion = "6.0.0"
	mongoFakeFCV     = "6.0"
)

// BuildAdminCommandReply returns a synthetic reply for the common pre-routing
// admin commands and reports whether cmd was one of them. clientAddr is the
// client's remote address, echoed back by whatsmyuri.
func BuildAdminCommandReply(cmd MongoCommand, clientAddr string) (BSON, bool) {
	switch strings.ToLower(cmd.CommandName()) {
	case "ping":
		return BSON{{Key: "ok", Value: 1.0}}, true
	case "buildinfo":
		return mongoBuildInfoReply(), true
	case "getparameter":
		return mongoGetParameterReply(cmd.Doc), true
	case "getlog":
		// Report no startup warnings; mongosh prints whatever this returns.
		return BSON{
			{Key: "totalLinesWritten", Value: int32(0)},
			{Key: "log", Value: []any{}},
			{Key: "ok", Value: 1.0},
		}, true
	case "whatsmyuri":
		return BSON{
			{Key: "you", Value: clientAddr},
			{Key: "ok", Value: 1.0},
		}, true
	case "connectionstatus":
		// Auth is advertised as disabled, so the connection carries no
		// authenticated users or roles.
		return BSON{
			{Key: "authInfo", Value: BSON{
				{Key: "authenticatedUsers", Value: []any{}},
				{Key: "authenticatedUserRoles", Value: []any{}},
			}},
			{Key: "ok", Value: 1.0},
		}, true
	case "getcmdlineopts":
		return BSON{
			{Key: "argv", Value: []any{}},
			{Key: "parsed", Value: BSON{}},
			{Key: "ok", Value: 1.0},
		}, true
	case "endsessions":
		return BSON{{Key: "ok", Value: 1.0}}, true
	default:
		return nil, false
	}
}

// mongoBuildInfoReply describes the proxy as a 64-bit MongoDB 6.0 standalone.
func mongoBuildInfoReply() BSON {
	return BSON{
		{Key: "version", Value: mongoFakeVersion},
		{Key: "versionArray", Value: []any{int32(6), int32(0), int32(0), int32(0)}},
		{Key: "gitVersion", Value: ""},
		{Key: "maxBsonObjectSize", Value: int32(mongoMaxBSONObjectSize)},
		{Key: "bits", Value: int32(64)},
		{Key: "debug", Value: false},
		{Key: "ok", Value: 1.0},
	}
}

// mongoGetParameterReply answers getParameter, supplying
// featureCompatibilityVersion when the request asks for it (mongosh always
// does during connect) or for all parameters ({getParameter: "*"}).
func mongoGetParameterReply(doc BSON) BSON {
	wantAll := false
	if v, ok := doc.Lookup("getParameter"); ok {
		if s, isStr := v.(string); isStr && s == "*" {
			wantAll = true
		}
	}
	out := BSON{}
	if _, ok := doc.Lookup("featureCompatibilityVersion"); ok || wantAll {
		out = append(out, BSONElem{Key: "featureCompatibilityVersion", Value: BSON{
			{Key: "version", Value: mongoFakeFCV},
		}})
	}
	return append(out, BSONElem{Key: "ok", Value: 1.0})
}
