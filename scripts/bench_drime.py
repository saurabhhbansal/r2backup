"""r2backup vs rclone on Drime: same data, same machine, same network, back to back.

Scenarios, each timed wall-clock for both tools:
  1. first upload of 1,000 small files (2-64 KB, 20 folders)
  2. first upload of 2 large files (128 MB each)
  3. re-run with nothing changed (the cost of every scheduled run)
  4. re-run after a typical day's edits: 20 files edited, 10 added,
     10 deleted, one folder (50 files) renamed
  5. same-size edit: 5 files rewritten with new content of identical size
     -- then a restore check of whether each tool's copy is current

r2b runs with its defaults. rclone runs `sync` with its defaults, and the
first-upload scenarios also with --transfers 16 (rclone's usual speed tip).
"""
import os, random, shutil, subprocess, sys, time, json, hashlib

# Configuration, from the environment:
#   R2B          path to the r2b binary under test
#   R2B_CREDS    a credentials file from a data directory set up for Drime
#                (r2b setup --provider drime with R2BACKUP_DATA_DIR pointing
#                somewhere disposable); copied into each run's own data dir
#   RCLONE_DEST  rclone remote and folder to sync into, e.g. drime:r2b-bench
#   BENCH_WORK   scratch directory for the generated data (default ./work)
# Afterwards remove the test data:
#   rclone purge $RCLONE_DEST --drime-hard-delete
#   rclone purge <drime-remote>:r2backup/machines/bench --drime-hard-delete
HERE = os.path.dirname(os.path.abspath(__file__))
WORK = os.environ.get("BENCH_WORK", os.path.join(HERE, "work"))
R2B = os.environ.get("R2B", "r2b")
SRC_CREDS = os.environ.get("R2B_CREDS", "")
REMOTE = os.environ.get("RCLONE_DEST", "drime:r2b-bench")
RESULTS = os.path.join(WORK, "results.json")

def log(*a):
    print(*a, flush=True)

def gen_small(root, seed=1):
    rnd = random.Random(seed)
    for i in range(1000):
        d = os.path.join(root, f"dir{i % 20:02d}", f"sub{i % 3}")
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, f"file{i:04d}.dat"), "wb") as f:
            f.write(rnd.randbytes(rnd.randint(2048, 65536)))

def gen_large(root, seed=2):
    rnd = random.Random(seed)
    os.makedirs(root, exist_ok=True)
    for i in range(2):
        with open(os.path.join(root, f"big{i}.bin"), "wb") as f:
            for _ in range(128):
                f.write(rnd.randbytes(1 << 20))

def mutate(root, seed=3):
    rnd = random.Random(seed)
    files = sorted(os.path.join(dp, f) for dp, _, fs in os.walk(root) for f in fs)
    for p in rnd.sample(files, 20):                         # 20 edited (grow)
        with open(p, "ab") as f:
            f.write(rnd.randbytes(1024))
    for i in range(10):                                     # 10 added
        with open(os.path.join(root, "dir00", f"new{i}.dat"), "wb") as f:
            f.write(rnd.randbytes(8192))
    rest = [p for p in files if os.path.exists(p) and "dir05" not in p]
    for p in rnd.sample(rest, 10):                          # 10 deleted
        os.remove(p)
    os.rename(os.path.join(root, "dir05"), os.path.join(root, "dir05-renamed"))  # ~50 files moved

def same_size_edit(root, seed=4):
    rnd = random.Random(seed)
    files = sorted(os.path.join(dp, f) for dp, _, fs in os.walk(root) for f in fs)
    picked = rnd.sample(files, 5)
    for p in picked:
        n = os.path.getsize(p)
        time.sleep(0.01)
        with open(p, "wb") as f:
            f.write(rnd.randbytes(n))
    return picked

def run(cmd, env=None):
    t0 = time.perf_counter()
    p = subprocess.run(cmd, env=env, capture_output=True, text=True, encoding="utf-8", errors="replace")
    dt = time.perf_counter() - t0
    if p.returncode != 0:
        log("   ! exit", p.returncode, (p.stderr or p.stdout)[-600:])
    return dt, p

def r2b_env(name):
    d = os.path.join(WORK, "r2bdata-" + name)
    os.makedirs(d, exist_ok=True)
    if not os.path.exists(os.path.join(d, "credentials")):
        shutil.copy(SRC_CREDS, os.path.join(d, "credentials"))
    e = dict(os.environ, R2BACKUP_DATA_DIR=d, R2BACKUP_NO_UPDATE_CHECK="1", R2BACKUP_MACHINE="bench")
    return e

def r2b_first(name, folder):
    return run([R2B, "--no", "add", folder, "--all", "--name", name], r2b_env(name))

def r2b_again(name):
    return run([R2B, "--no", "backup", name], r2b_env(name))

def rclone(folder, dest, *extra):
    return run(["rclone", "sync", folder, f"{REMOTE}/{dest}", "--stats-one-line", "--stats", "0", *extra])

def main():
    if not SRC_CREDS:
        sys.exit("set R2B_CREDS to a Drime-configured r2b credentials file; see the top of this file")
    if os.path.exists(WORK):
        shutil.rmtree(WORK)
    os.makedirs(WORK)
    res = {}
    def rec(k, v):
        res[k] = round(v, 2)
        log(f"{k:45s} {v:8.1f} s")
        json.dump(res, open(RESULTS, "w"), indent=1)

    small = {t: os.path.join(WORK, f"small-{t}") for t in ("r2b", "rc", "rc16")}
    large = {t: os.path.join(WORK, f"large-{t}") for t in ("r2b", "rc", "rc16")}
    for t in small:
        gen_small(small[t]); gen_large(large[t])
    log("data ready: 1,000 small files per copy, 2 x 128 MB per copy")

    # 1. first upload, small files
    rec("1 small first upload: r2b", r2b_first("small", small["r2b"])[0])
    rec("1 small first upload: rclone", rclone(small["rc"], "rc-small")[0])
    rec("1 small first upload: rclone --transfers 16", rclone(small["rc16"], "rc16-small", "--transfers", "16")[0])

    # 2. first upload, large files
    rec("2 large first upload: r2b", r2b_first("large", large["r2b"])[0])
    rec("2 large first upload: rclone", rclone(large["rc"], "rc-large")[0])
    rec("2 large first upload: rclone --transfers 16", rclone(large["rc16"], "rc16-large", "--transfers", "16")[0])

    # 3. nothing changed
    rec("3 no-change re-run: r2b", r2b_again("small")[0])
    rec("3 no-change re-run: rclone", rclone(small["rc"], "rc-small")[0])

    # 4. a day's edits incl. a folder rename
    mutate(small["r2b"]); mutate(small["rc"])
    rec("4 edits + rename re-run: r2b", r2b_again("small")[0])
    rec("4 edits + rename re-run: rclone", rclone(small["rc"], "rc-small")[0])

    # 5. same-size edits: does each tool notice?
    a = same_size_edit(small["r2b"]); b = same_size_edit(small["rc"])
    rec("5 same-size edit re-run: r2b", r2b_again("small")[0])
    rec("5 same-size edit re-run: rclone", rclone(small["rc"], "rc-small")[0])
    # check: are the edited files current remotely? compare via download hash
    def remote_ok_rclone(paths, root, dest):
        ok = 0
        for p in paths:
            rel = os.path.relpath(p, root).replace("\\", "/")
            out = subprocess.run(["rclone", "cat", f"{REMOTE}/{dest}/{rel}"], capture_output=True).stdout
            ok += hashlib.sha256(out).digest() == hashlib.sha256(open(p, "rb").read()).digest()
        return ok
    res["5 same-size edits current on remote: rclone"] = f"{remote_ok_rclone(b, small['rc'], 'rc-small')}/5"
    restore = os.path.join(WORK, "restore-r2b")
    run([R2B, "--no", "restore", "small", "--to", restore], r2b_env("small"))
    ok = 0
    for p in a:
        rel = os.path.relpath(p, small["r2b"])
        q = os.path.join(restore, rel)
        ok += os.path.exists(q) and open(q, "rb").read() == open(p, "rb").read()
    res["5 same-size edits current on remote: r2b"] = f"{ok}/5"
    log("same-size edits current: r2b", res["5 same-size edits current on remote: r2b"],
        " rclone", res["5 same-size edits current on remote: rclone"])
    json.dump(res, open(RESULTS, "w"), indent=1)
    log("done")

if __name__ == "__main__":
    main()
