"""r2backup vs rclone on Drime: same data, same machine, same network, back to back.

Datasets (random, incompressible content):
  tiny    10,000 files of 1-16 KB in 100 folders      (~85 MB)
  docs     1,000 files of 100 KB-5 MB, log-uniform    (~1.2 GB)
  photos     200 files of 3-8 MB                      (~1.1 GB)
  large        2 files of 512 MB                      (1 GB)

Steps, run one at a time so the person running it can stop between them:
  gen                  generate all four datasets under BENCH_WORK
  first <dataset>      first upload: r2b, then rclone sync --transfers 16
                       (add --rclone-default to also time rclone's default 4)
  r2bfirst <dataset> [label]  first upload with r2b only
  rcfirst <dataset> N  first upload with rclone only, at --transfers N
                       (and --checkers N); purges rclone's copy first
  nochange <dataset>   re-run with nothing changed, both tools
  rctimed <dataset> N M  rclone only, --transfers N, stopped after M minutes;
                       records how many files reached Drime
  edits <dataset>      tiny: 200 files edited, 100 added, 100 deleted, one
                       folder (~100 files) renamed; docs: 20/10/10, one folder
                       (~50 files) renamed. Re-run both tools
  samesize <dataset>   20 files rewritten at the same size; re-run both tools,
                       then check which tool's copy is current
  purge <dataset>      hard-delete both tools' copies of a dataset on Drime

r2b runs with its defaults (32 parallel uploads, adapting between 4 and
128). rclone's re-runs use RCLONE_TRANSFERS (default 16) for --transfers and
--checkers. Results are appended to BENCH_WORK/results.json.

Configuration, from the environment:
  R2B          path to the r2b binary under test
  R2B_CREDS    a Drime-configured r2b credentials file (on Windows it is
               sealed to the user, so copy one from the same account)
  RCLONE_DRIME rclone Drime remote name (default "drime"); set hard_delete on
               it so purges skip the trash
  BENCH_WORK   scratch directory for data and r2b state. Keep it outside any
               folder you back up.
"""
import hashlib, json, math, os, random, shutil, subprocess, sys, time

WORK = os.environ.get("BENCH_WORK") or sys.exit("set BENCH_WORK")
R2B = os.environ.get("R2B", "r2b")
SRC_CREDS = os.environ.get("R2B_CREDS", "")
RC = os.environ.get("RCLONE_DRIME", "drime")
RESULTS = os.path.join(WORK, "results.json")
DATA = os.path.join(WORK, "data")
R2B_DATA = os.path.join(WORK, "r2bdata")
MACHINE = "bench"
DATASETS = ("tiny", "docs", "photos", "large")


def log(*a):
    print(*a, flush=True)


def write_random(path, n):
    with open(path, "wb") as f:
        while n > 0:
            k = min(n, 1 << 22)
            f.write(os.urandom(k))
            n -= k


def gen(name):
    root = os.path.join(DATA, name)
    if os.path.exists(root):
        shutil.rmtree(root)
    rnd = random.Random(name)
    if name == "tiny":
        for i in range(10000):
            d = os.path.join(root, f"dir{i % 100:02d}", f"sub{i % 4}")
            os.makedirs(d, exist_ok=True)
            write_random(os.path.join(d, f"file{i:05d}.dat"), rnd.randint(1024, 16384))
    elif name == "docs":
        for i in range(1000):
            d = os.path.join(root, f"project{i % 20:02d}")
            os.makedirs(d, exist_ok=True)
            n = int(math.exp(rnd.uniform(math.log(100_000), math.log(5_000_000))))
            write_random(os.path.join(d, f"doc{i:04d}.bin"), n)
    elif name == "photos":
        for i in range(200):
            d = os.path.join(root, f"2026-{i % 4 + 1:02d}")
            os.makedirs(d, exist_ok=True)
            write_random(os.path.join(d, f"IMG_{i:04d}.jpg"), rnd.randint(3_000_000, 8_000_000))
    elif name == "large":
        os.makedirs(root)
        for i in range(2):
            write_random(os.path.join(root, f"big{i}.bin"), 512 << 20)
    files = [os.path.join(dp, f) for dp, _, fs in os.walk(root) for f in fs]
    log(f"{name}: {len(files)} files, {sum(map(os.path.getsize, files)) / 1e6:.0f} MB")


def run(cmd, env=None):
    t0 = time.perf_counter()
    p = subprocess.run(cmd, env=env, capture_output=True, text=True, encoding="utf-8", errors="replace")
    dt = time.perf_counter() - t0
    out = (p.stdout + p.stderr).strip().splitlines()
    log("   ", " | ".join(out[-3:])[-400:])
    if p.returncode != 0:
        log("   ! exit", p.returncode)
    return dt, p.returncode


def r2b_env():
    os.makedirs(R2B_DATA, exist_ok=True)
    dst = os.path.join(R2B_DATA, "credentials")
    if not os.path.exists(dst):
        if not SRC_CREDS:
            sys.exit("set R2B_CREDS")
        shutil.copy(SRC_CREDS, dst)
    return dict(os.environ, R2BACKUP_DATA_DIR=R2B_DATA, R2BACKUP_NO_UPDATE_CHECK="1", R2BACKUP_MACHINE=MACHINE)


def r2b_first(name):
    return run([R2B, "--no", "add", os.path.join(DATA, name), "--all", "--name", f"bench-{name}"], r2b_env())


def r2b_again(name):
    return run([R2B, "--no", "backup", f"bench-{name}"], r2b_env())


def rclone_sync(name, *extra):
    return run(["rclone", "sync", os.path.join(DATA, name), f"{RC}:bench-rclone/{name}", "--stats-one-line", "--stats", "0", *extra])


def par(n):
    # rclone's parallel uploads and its parallel listings/checks, both at n.
    return ("--transfers", str(n), "--checkers", str(n))


RC_N = int(os.environ.get("RCLONE_TRANSFERS", "16"))


def r2b_remote(name):
    return f"{RC}:r2backup/machines/{MACHINE}/bench-{name}/current"


def record(key, value):
    res = json.load(open(RESULTS)) if os.path.exists(RESULTS) else {}
    # Keep earlier runs: a repeat gets "(run N)" instead of overwriting.
    base, n = key, 2
    while key in res:
        key, n = f"{base} (run {n})", n + 1
    res[key] = round(value, 1) if isinstance(value, float) else value
    json.dump(res, open(RESULTS, "w"), indent=1)
    log(f"{key:50s} {res[key]}")


def timed(key, result):
    dt, code = result
    record(key, dt if code == 0 else f"failed after {dt:.0f} s (exit {code})")


def all_files(root):
    return sorted(os.path.join(dp, f) for dp, _, fs in os.walk(root) for f in fs)


# How much a "day's edits" touches, per dataset: edited, added, deleted, and
# the folder that gets renamed.
EDITS = {"tiny": (200, 100, 100, "dir07"), "docs": (20, 10, 10, "project07")}


def edits(name):
    root = os.path.join(DATA, name)
    n_edit, n_add, n_del, folder = EDITS[name]
    rnd = random.Random("edits")
    files = all_files(root)
    for p in rnd.sample(files, n_edit):
        with open(p, "ab") as f:
            f.write(os.urandom(1024))
    first = sorted(os.listdir(root))[0]
    for i in range(n_add):
        write_random(os.path.join(root, first, f"new{i:03d}.dat"), 8192)
    rest = [p for p in files if f"{os.sep}{folder}{os.sep}" not in p]
    for p in rnd.sample(rest, n_del):
        os.remove(p)
    os.rename(os.path.join(root, folder), os.path.join(root, folder + "-renamed"))


def same_size(name):
    root = os.path.join(DATA, name)
    rnd = random.Random("samesize")
    picked = rnd.sample(all_files(root), 20)
    for p in picked:
        n = os.path.getsize(p)
        write_random(p, n)
    return [os.path.relpath(p, root).replace("\\", "/") for p in picked]


def current_count(name, rels, remote):
    root = os.path.join(DATA, name)
    ok = 0
    for rel in rels:
        got = subprocess.run(["rclone", "cat", f"{remote}/{rel}"], capture_output=True).stdout
        ok += hashlib.sha256(got).digest() == hashlib.sha256(open(os.path.join(root, rel), "rb").read()).digest()
    return f"{ok}/{len(rels)}"


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    step, arg = sys.argv[1], (sys.argv[2] if len(sys.argv) > 2 else None)
    os.makedirs(WORK, exist_ok=True)
    if step == "gen":
        for d in ([arg] if arg else DATASETS):
            gen(d)
    elif step == "first":
        timed(f"{arg} first upload: r2b", r2b_first(arg))
        timed(f"{arg} first upload: rclone --transfers 16", rclone_sync(arg, "--transfers", "16"))
        if "--rclone-default" in sys.argv:
            run(["rclone", "purge", f"{RC}:bench-rclone/{arg}"])
            timed(f"{arg} first upload: rclone default", rclone_sync(arg))
    elif step == "r2bfirst":
        timed(f"{arg} first upload: r2b ({sys.argv[3] if len(sys.argv) > 3 else 'again'})", r2b_first(arg))
    elif step == "nochange":
        timed(f"{arg} no-change re-run: r2b", r2b_again(arg))
        timed(f"{arg} no-change re-run: rclone --transfers {RC_N}", rclone_sync(arg, *par(RC_N)))
    elif step == "rcfirst":
        n = int(sys.argv[3])
        run(["rclone", "purge", f"{RC}:bench-rclone/{arg}"])
        timed(f"{arg} first upload: rclone --transfers {n}", rclone_sync(arg, *par(n)))
    elif step == "edits":
        edits(arg)
        timed(f"{arg} edits + rename re-run: r2b", r2b_again(arg))
        timed(f"{arg} edits + rename re-run: rclone --transfers {RC_N}", rclone_sync(arg, *par(RC_N)))
    elif step == "samesize":
        rels = same_size(arg)
        timed(f"{arg} same-size edit re-run: r2b", r2b_again(arg))
        timed(f"{arg} same-size edit re-run: rclone --transfers {RC_N}", rclone_sync(arg, *par(RC_N)))
        record(f"{arg} same-size edits current on Drime: r2b", current_count(arg, rels, r2b_remote(arg)))
        record(f"{arg} same-size edits current on Drime: rclone", current_count(arg, rels, f"{RC}:bench-rclone/{arg}"))
    elif step == "rctimed":
        # For sets too slow to finish: run rclone for a fixed time, then count
        # what reached Drime.
        n, mins = int(sys.argv[3]), int(sys.argv[4])
        run(["rclone", "purge", f"{RC}:bench-rclone/{arg}"])
        dt, _ = rclone_sync(arg, *par(n), "--max-duration", f"{mins}m")
        got = subprocess.run(["rclone", "lsf", "-R", "--files-only", f"{RC}:bench-rclone/{arg}"],
                             capture_output=True, text=True).stdout.count(chr(10))
        record(f"{arg} first upload: rclone --transfers {n}, files in {dt:.0f} s", f"{got} ({got / dt:.2f} files/s)")
    elif step == "purge":
        run(["rclone", "purge", f"{RC}:bench-rclone/{arg}"])
        run(["rclone", "purge", f"{RC}:r2backup/machines/{MACHINE}/bench-{arg}"])
        run([R2B, "--no", "remove", f"bench-{arg}"], r2b_env())
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
