package historycompat

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "errors"
 "os"
 "path/filepath"
 "runtime"
 "testing"
)
type scannerFunc func(context.Context)error
func(f scannerFunc)CheckStopped(c context.Context)error{return f(c)}
type evidenceFunc func(context.Context,string,string)(string,error)
func(f evidenceFunc)Fingerprint(c context.Context,h,p string)(string,error){return f(c,h,p)}
func fixture(t *testing.T)(Request,string,string){
 t.Helper();if runtime.GOOS!="linux"{t.Skip("Linux-only preflight prototype")};root:=t.TempDir()
 home,project,system:=filepath.Join(root,"home"),filepath.Join(root,"project"),filepath.Join(root,"system")
 for _,p:=range []string{home,project,system}{if err:=os.Mkdir(p,0700);err!=nil{t.Fatal(err)}}
 artifact:=func(name string)Artifact{p:=filepath.Join(root,name);b:=[]byte("synthetic nonexecuted "+name);if err:=os.WriteFile(p,b,0700);err!=nil{t.Fatal(err)};h:=sha256.Sum256(b);return Artifact{Path:p,Pin:ArtifactPin{Version:NativeVersion,SHA256:hex.EncodeToString(h[:])}}}
 return Request{StoppedWritersAcknowledged:true,ProfileHome:home,ProjectRoot:project,Native:artifact("native"),Helper:artifact("helper"),Scanner:scannerFunc(func(context.Context)error{return nil}),Configuration:evidenceFunc(func(context.Context,string,string)(string,error){return "synthetic-evidence",nil})},system,root
}
func run(t *testing.T,r Request,system,root string)(*Snapshot,error){t.Helper();return check(context.Background(),r,"linux",system,root)}
func TestSyntheticPreflightAndRepeat(t *testing.T){r,sys,root:=fixture(t);s,err:=run(t,r,sys,root);if err!=nil{t.Fatal(err)};n,err:=run(t,r,sys,root);if err!=nil{t.Fatal(err)};if err=s.equal(n);err!=nil{t.Fatal(err)};if _,err=os.Stat(filepath.Join(r.ProfileHome,"thread_history_1.sqlite"));!os.IsNotExist(err){t.Fatal("preflight created database")}}
func TestRejectBeforeScanner(t *testing.T){
 cases:=map[string]func(*Request){"ack":func(r *Request){r.StoppedWritersAcknowledged=false},"native-version":func(r *Request){r.Native.Pin.Version="0.165.0"},"helper-version":func(r *Request){r.Helper.Pin.Version="0.167.0"},"digest":func(r *Request){r.Native.Pin.SHA256="bad"},"wrong-digest":func(r *Request){r.Native.Pin.SHA256=r.Helper.Pin.SHA256},"sqlite-env":func(r *Request){r.Environment=[]string{"CODEX_SQLITE_HOME="}},"home-env":func(r *Request){r.Environment=[]string{"CODEX_HOME=/different"}},"duplicate-env":func(r *Request){r.Environment=[]string{"X=1","X=2"}},"flag":func(r *Request){r.Args=[]string{"-c","sqlite_home='/other'"}},"subcommand":func(r *Request){r.Args=[]string{"app-server"}},"missing-evidence":func(r *Request){r.Configuration=nil}}
 for name,mutate:=range cases{t.Run(name,func(t *testing.T){r,sys,root:=fixture(t);r.Scanner=scannerFunc(func(context.Context)error{t.Fatal("scanner called before rejection");return nil});mutate(&r);if _,err:=run(t,r,sys,root);err==nil{t.Fatal("accepted")}})}
}
func TestUnsupportedOS(t *testing.T){r,sys,root:=fixture(t);for _,osname:=range []string{"darwin","windows"}{if _,err:=check(context.Background(),r,osname,sys,root);err==nil{t.Fatal("accepted",osname)}}}
func TestInspectionFailures(t *testing.T){for _,which:=range []string{"scanner","configuration","empty-evidence","nil-scanner"}{t.Run(which,func(t *testing.T){r,sys,root:=fixture(t);switch which{case "scanner":r.Scanner=scannerFunc(func(context.Context)error{return errors.New("incomplete inspection")});case "configuration":r.Configuration=evidenceFunc(func(context.Context,string,string)(string,error){return "",errors.New("unknown cloud policy")});case "empty-evidence":r.Configuration=evidenceFunc(func(context.Context,string,string)(string,error){return "",nil});case "nil-scanner":r.Scanner=nil};if _,err:=run(t,r,sys,root);err==nil{t.Fatal("accepted")}})}}
func TestStructuralTOML(t *testing.T){for _,body:=range []string{"'sqlite_home' = '/other'\n","[profiles.custom]\nmodel='x'\n","profile='custom'\n","[config_server]\nurl='x'\n","broken = ["}{t.Run(body,func(t *testing.T){r,sys,root:=fixture(t);os.WriteFile(filepath.Join(r.ProfileHome,"config.toml"),[]byte(body),0600);if _,err:=run(t,r,sys,root);err==nil{t.Fatal("accepted")}})};r,sys,root:=fixture(t);os.WriteFile(filepath.Join(r.ProfileHome,"config.toml"),[]byte("model='synthetic'\n"),0600);if _,err:=run(t,r,sys,root);err!=nil{t.Fatal(err)}}
func TestForbiddenLayers(t *testing.T){for _,name:=range []string{"config.toml","requirements.toml","managed_config.toml","project","ancestor"}{t.Run(name,func(t *testing.T){r,sys,root:=fixture(t);p:=filepath.Join(sys,name);if name=="project"{p=filepath.Join(r.ProjectRoot,"config.toml")};if name=="ancestor"{p=filepath.Join(root,".codex","config.toml");os.Mkdir(filepath.Dir(p),0700)};os.WriteFile(p,[]byte("model='x'"),0600);if _,err:=run(t,r,sys,root);err==nil{t.Fatal("accepted")}})}}
func TestSymlinkAndHardlinkRejected(t *testing.T){for _,kind:=range []string{"symlink","hardlink","parent"}{t.Run(kind,func(t *testing.T){r,sys,root:=fixture(t);if kind=="parent"{alias:=filepath.Join(root,"alias");if err:=os.Symlink(r.ProfileHome,alias);err!=nil{t.Fatal(err)};r.ProfileHome=alias}else{p:=filepath.Join(root,"alias");var err error;if kind=="symlink"{err=os.Symlink(r.Native.Path,p);r.Native.Path=p}else{err=os.Link(r.Native.Path,p)};if err!=nil{t.Fatal(err)}};if _,err:=run(t,r,sys,root);err==nil{t.Fatal("accepted")}})}}
func TestSnapshotDetectsChangedOrNewConfig(t *testing.T){r,sys,root:=fixture(t);s,err:=run(t,r,sys,root);if err!=nil{t.Fatal(err)};os.WriteFile(filepath.Join(r.ProfileHome,"config.toml"),[]byte("model='x'"),0600);if err=s.verifyFiles();err==nil{t.Fatal("new config missed")}}
func TestSnapshotDetectsSameBytesNewIdentity(t *testing.T){r,sys,root:=fixture(t);s,err:=run(t,r,sys,root);if err!=nil{t.Fatal(err)};b,err:=os.ReadFile(r.Native.Path);if err!=nil{t.Fatal(err)};tmp:=r.Native.Path+".replacement";os.WriteFile(tmp,b,0700);if err=os.Rename(tmp,r.Native.Path);err!=nil{t.Fatal(err)};if err=s.verifyFiles();err==nil{t.Fatal("replacement missed")}}
func TestMutationDuringInspectionRejected(t *testing.T){r,sys,root:=fixture(t);r.Scanner=scannerFunc(func(context.Context)error{return os.WriteFile(r.Native.Path,[]byte("changed"),0700)});if _,err:=run(t,r,sys,root);err==nil{t.Fatal("mutation missed")}}
func TestSnapshotDetectsLaunchChanges(t *testing.T){r,sys,root:=fixture(t);s,err:=run(t,r,sys,root);if err!=nil{t.Fatal(err)};r.Environment=[]string{"SYNTHETIC=value"};n,err:=run(t,r,sys,root);if err!=nil{t.Fatal(err)};if err=s.equal(n);err==nil{t.Fatal("environment change missed")}}
func TestSnapshotDetectsDirectoryReplacement(t *testing.T){r,sys,root:=fixture(t);s,err:=run(t,r,sys,root);if err!=nil{t.Fatal(err)};if err=os.Rename(r.ProfileHome,r.ProfileHome+".old");err!=nil{t.Fatal(err)};if err=os.Mkdir(r.ProfileHome,0700);err!=nil{t.Fatal(err)};if err=s.verifyFiles();err==nil{t.Fatal("directory replacement missed")}}
