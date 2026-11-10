#!/usr/bin/python3
"""Account-free app-server fixture. Not a language model or a real Codex CLI."""
import json, os, pathlib, subprocess, sys, time, urllib.request

def emit(value):
    print(json.dumps(value), flush=True)

if "--version" in sys.argv:
    print("codex-cli 0.156.1")
    sys.exit(0)
if "--help" in sys.argv:
    sys.exit(0)
thread_id = "fixture-"+__import__("uuid").uuid4().hex
root = pathlib.Path.cwd()
thread = dict(id=thread_id,sessionId=thread_id,projectId=None,forkedFromId=None,
    preview="",ephemeral=False,modelProvider="openai",createdAt=int(time.time()),
    updatedAt=int(time.time()),status={"type":"idle"},path=str(root/"fixture.jsonl"),
    cwd=str(root),cliVersion="0.156.1",source="vscode",threadSource=None,
    agentNickname=None,agentRole=None,gitInfo=None,name=None,turns=[])
turn_count=0
active_turn=None
heartbeat_started=False
def heartbeat():
    global heartbeat_started
    if heartbeat_started: return
    heartbeat_started=True
    if os.fork() == 0:
        os.setsid()
        for fd in [0,1,2]: os.close(fd)
        while True:
            (root/"fixture-heartbeat").write_text(str(time.monotonic_ns()))
            time.sleep(0.02)
def finish_turn(turn):
    turn_id=turn["id"]
    stage="probe-config"
    try:
        if "fail-fixture" in json.dumps(params): raise ValueError("requested fixture failure")
        # Probe only explicitly generated fixtures, never real account data.
        paths=json.loads((pathlib.Path(__file__).parent/"probe-paths.json").read_text())
        for i,path in enumerate(paths):
            stage="probe-read-"+str(i)
            try:pathlib.Path(path).read_bytes()
            except OSError:pass
            else:raise AssertionError("fixture outside task readable")
            stage="probe-write-"+str(i)
            try:pathlib.Path(path).write_text("tamper")
            except OSError:pass
            # denyRead directories can be private writable tmpfs shadows.
            # The Go harness separately checks host fixtures are unchanged.
        stage="network-deny"
        try: urllib.request.urlopen("https://example.com", timeout=3)
        except OSError: pass
        else: raise AssertionError("fixture network allowed")
        stage="write-task"
        (root/"task-edit.txt").write_text("edited by fake provider turn "+str(turn_count)+"\n")
        stage="git-add"
        subprocess.run(["/usr/bin/git","add","task-edit.txt"],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        stage="git-commit"
        subprocess.run(["/usr/bin/git","commit","-m","fixture task commit"],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        sha=subprocess.check_output(["/usr/bin/git","rev-parse","HEAD"],text=True).strip()
        text="Edited task-edit.txt, committed, and verified outside-fixture confinement.\nSTATUS: PASS\nCOMMIT: "+sha+"\nEVIDENCE: fixture confinement/network checks PASS"
        item=dict(id="fixture-item-"+str(turn_count),type="agentMessage",text=text,phase="final_answer")
        emit({"method":"item/started","params":{"threadId":thread_id,"turnId":turn_id,"item":item}})
        emit({"method":"item/agentMessage/delta","params":{"threadId":thread_id,"turnId":turn_id,"itemId":item["id"],"delta":text}})
        emit({"method":"item/completed","params":{"threadId":thread_id,"turnId":turn_id,"item":item}})
        turn.update(status="completed",items=[item]);thread["turns"].append(turn)
    except Exception as e:
        (root/"fixture-error").write_text(stage+":"+type(e).__name__)
        turn.update(status="failed",error={"message":"fixture operation failed","codexErrorInfo":None,"additionalDetails":None})
    emit({"method":"turn/completed","params":{"threadId":thread_id,"turn":turn}})
for line in sys.stdin:
    request=json.loads(line);method=request.get("method","");params=request.get("params") or {}
    with (root/"fixture-methods").open("a") as f:f.write(method+"\n")
    if "id" not in request:continue
    result={}
    if method=="initialize":
        heartbeat()
        result=dict(userAgent="codex-cli/0.156.1",codexHome=os.environ.get("HOME"),platformFamily="unix",platformOs="linux")
    elif method=="account/read":result={"account":{"type":"apiKey"},"requiresOpenaiAuth":False}
    elif method=="account/rateLimits/read":result={"rateLimits":{"primary":None,"secondary":None,"credits":None,"planType":"plus"},"rateLimitsByLimitId":None}
    elif method=="model/list":result={"data":[dict(id="gpt-5.4",model="gpt-5.4",displayName="Fixture",description="Fixture",hidden=False,isDefault=True,supportedReasoningEfforts=[dict(reasoningEffort="medium",description="Fixture")],defaultReasoningEffort="medium",inputModalities=["text"],supportsPersonality=False)],"nextCursor":None}
    elif method in ["thread/start","thread/resume","thread/fork"]:
        result=dict(thread=thread,model="gpt-5.4",modelProvider="openai",serviceTier=None,cwd=str(root),instructionSources=[],approvalPolicy="never",approvalsReviewer="user",sandbox={"type":"dangerFullAccess"},reasoningEffort="medium")
    elif method=="thread/read":result={"thread":thread}
    elif method=="turn/steer":
        result={"turnId":active_turn["id"] if active_turn else "fixture-turn-1"}
        if active_turn and "finish-fixture" in json.dumps(params):
            emit({"id":request["id"],"result":result})
            finish_turn(active_turn)
            active_turn=None
            continue
    elif method=="turn/interrupt":
        emit({"id":request["id"],"result":{}})
        if active_turn:
            active_turn.update(status="interrupted")
            emit({"method":"turn/completed","params":{"threadId":thread_id,"turn":active_turn}})
            active_turn=None
        continue
    elif method=="turn/start":
        turn_count+=1;turn_id="fixture-turn-"+str(turn_count)
        turn=dict(id=turn_id,items=[],status="inProgress",error=None)
        emit({"id":request["id"],"result":{"turn":turn}})
        emit({"method":"turn/started","params":{"threadId":thread_id,"turn":turn}})
        if "hold-fixture" in json.dumps(params):
            active_turn=turn
            # The initialize-time helper escapes the outer process group but
            # not bwrap's PID namespace; it must die on a confirmed stop.
            (root/"fixture-held").write_text("held")
            continue
        finish_turn(turn)
        continue
    emit({"id":request["id"],"result":result})
