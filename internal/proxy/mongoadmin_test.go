package proxy

import "testing"

// cmd builds a MongoCommand whose document begins with the named command, the
// shape BuildAdminCommandReply dispatches on.
func adminCmd(name string, rest ...BSONElem) MongoCommand {
	doc := BSON{{Key: name, Value: int32(1)}}
	doc = append(doc, rest...)
	return MongoCommand{Doc: doc, DB: "admin", OpCode: OpMsg}
}

func TestBuildAdminCommandReply_HandledCommands(t *testing.T) {
	cases := []struct {
		name string
		cmd  MongoCommand
		// check inspects the reply for command-specific fields beyond ok:1.
		check func(t *testing.T, reply BSON)
	}{
		{
			name: "ping",
			cmd:  adminCmd("ping"),
		},
		{
			name: "buildInfo",
			cmd:  adminCmd("buildInfo"),
			check: func(t *testing.T, reply BSON) {
				if v, _ := lookupBSONString(reply, "version"); v != mongoFakeVersion {
					t.Errorf("version = %q, want %q", v, mongoFakeVersion)
				}
				arr, ok := reply.Lookup("versionArray")
				if !ok {
					t.Fatal("buildInfo missing versionArray")
				}
				if a, _ := arr.([]any); len(a) != 4 || a[0] != int32(6) {
					t.Errorf("versionArray = %v, want [6 0 0 0]", arr)
				}
			},
		},
		{
			name: "buildinfo lowercase alias",
			cmd:  adminCmd("buildinfo"),
		},
		{
			name: "getParameter with FCV",
			cmd:  adminCmd("getParameter", BSONElem{Key: "featureCompatibilityVersion", Value: int32(1)}),
			check: func(t *testing.T, reply BSON) {
				v, ok := reply.Lookup("featureCompatibilityVersion")
				if !ok {
					t.Fatal("getParameter reply missing featureCompatibilityVersion")
				}
				sub, _ := v.(BSON)
				if ver, _ := lookupBSONString(sub, "version"); ver != mongoFakeFCV {
					t.Errorf("FCV version = %q, want %q", ver, mongoFakeFCV)
				}
			},
		},
		{
			name: "getLog",
			cmd:  adminCmd("getLog", BSONElem{Key: "getLog", Value: "startupWarnings"}),
			check: func(t *testing.T, reply BSON) {
				arr, ok := reply.Lookup("log")
				if !ok {
					t.Fatal("getLog reply missing log")
				}
				if a, _ := arr.([]any); len(a) != 0 {
					t.Errorf("log = %v, want empty", arr)
				}
			},
		},
		{
			name: "connectionStatus",
			cmd:  adminCmd("connectionStatus"),
			check: func(t *testing.T, reply BSON) {
				v, ok := reply.Lookup("authInfo")
				if !ok {
					t.Fatal("connectionStatus reply missing authInfo")
				}
				info, _ := v.(BSON)
				users, ok := info.Lookup("authenticatedUsers")
				if !ok {
					t.Fatal("authInfo missing authenticatedUsers")
				}
				if a, _ := users.([]any); len(a) != 0 {
					t.Errorf("authenticatedUsers = %v, want empty", users)
				}
			},
		},
		{
			name: "getCmdLineOpts",
			cmd:  adminCmd("getCmdLineOpts"),
		},
		{
			name: "endSessions",
			cmd:  adminCmd("endSessions"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, ok := BuildAdminCommandReply(tc.cmd, "127.0.0.1:55555")
			if !ok {
				t.Fatalf("command %q not handled", tc.cmd.CommandName())
			}
			if !mongoReplyOK(reply) {
				t.Errorf("reply not ok:1: %+v", reply)
			}
			if tc.check != nil {
				tc.check(t, reply)
			}
		})
	}
}

func TestBuildAdminCommandReply_Whatsmyuri(t *testing.T) {
	reply, ok := BuildAdminCommandReply(adminCmd("whatsmyuri"), "10.0.0.7:42")
	if !ok {
		t.Fatal("whatsmyuri not handled")
	}
	if you, _ := lookupBSONString(reply, "you"); you != "10.0.0.7:42" {
		t.Errorf("you = %q, want client addr", you)
	}
}

func TestBuildAdminCommandReply_GetParameterAll(t *testing.T) {
	// {getParameter: "*"} should surface featureCompatibilityVersion too.
	cmd := MongoCommand{Doc: BSON{{Key: "getParameter", Value: "*"}}, DB: "admin", OpCode: OpMsg}
	reply, ok := BuildAdminCommandReply(cmd, "")
	if !ok {
		t.Fatal("getParameter not handled")
	}
	if _, ok := reply.Lookup("featureCompatibilityVersion"); !ok {
		t.Error("getParameter:* should include featureCompatibilityVersion")
	}
}

func TestBuildAdminCommandReply_Unhandled(t *testing.T) {
	// Real operations and Atlas-only probes are left to the caller's error path.
	for _, name := range []string{"find", "insert", "atlasVersion", "listDatabases", ""} {
		cmd := MongoCommand{Doc: BSON{{Key: name, Value: int32(1)}}, OpCode: OpMsg}
		if _, ok := BuildAdminCommandReply(cmd, ""); ok {
			t.Errorf("command %q should not be handled locally", name)
		}
	}
}
