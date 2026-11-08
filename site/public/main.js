import * as THREE from "three";

const root = document.documentElement;
const $ = (id) => document.getElementById(id);
const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
const systemDark = window.matchMedia("(prefers-color-scheme: dark)");

/* -------------------------------------------------------------- theme -- */

function isDark() {
  const t = root.dataset.theme;
  return t ? t === "dark" : systemDark.matches;
}

const themeButtons = document.querySelectorAll("[data-theme-set]");
function showThemeChoice() {
  const choice = root.dataset.theme || "system";
  themeButtons.forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.themeSet === choice)));
}
themeButtons.forEach((b) => {
  b.addEventListener("click", () => {
    const choice = b.dataset.themeSet;
    if (choice === "system") delete root.dataset.theme;
    else root.dataset.theme = choice;
    try {
      if (choice === "system") localStorage.removeItem("theme");
      else localStorage.setItem("theme", choice);
    } catch { /* private window: the choice lasts for this page only */ }
    showThemeChoice();
    window.dispatchEvent(new Event("r2b-theme"));
  });
});
systemDark.addEventListener("change", () => window.dispatchEvent(new Event("r2b-theme")));
showThemeChoice();

/* ---------------------------------------------------------------- scene -- */

// The page's two inks. Primary (Drime) files land in full ink; Secondary (S3)
// files settle into grey, the same order of importance the copy gives them.
const PALETTE = {
  dark: {
    bg: "#000000", shell: "#151515", edge: "#ededed", edgeOpacity: 0.24,
    ink: "#ededed", sub: "#8f8f8f", dim: "#4a4a4a", floor: "#2b2b2b",
    sky: "#ffffff", ground: "#000000", hemi: 0.5, key: 1.2, rim: 1.5, shadow: 0,
  },
  light: {
    bg: "#ffffff", shell: "#f4f4f4", edge: "#171717", edgeOpacity: 0.3,
    ink: "#171717", sub: "#7a7a7a", dim: "#c4c4c4", floor: "#cdcdcd",
    sky: "#ffffff", ground: "#bdbdbd", hemi: 1.15, key: 0.9, rim: 0.5, shadow: 0.16,
  },
};

function startScene(canvas) {
  const renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: true, powerPreference: "high-performance" });
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.setClearColor(0x000000, 0);
  renderer.outputColorSpace = THREE.SRGBColorSpace;
  renderer.toneMapping = THREE.ACESFilmicToneMapping;
  renderer.toneMappingExposure = 1.05;

  const scene = new THREE.Scene();
  scene.fog = new THREE.Fog("#000000", 11, 23);

  const camera = new THREE.PerspectiveCamera(30, 1, 0.1, 100);
  const camBase = new THREE.Vector3(9.4, 4.6, 9.4);
  camera.position.copy(camBase);

  const hemi = new THREE.HemisphereLight("#ffffff", "#000000", 0.5);
  scene.add(hemi);
  const key = new THREE.DirectionalLight("#ffffff", 1.2);
  key.position.set(6, 6, -2);
  scene.add(key);
  const rim = new THREE.DirectionalLight("#ffffff", 1.5);
  rim.position.set(-6, 3, -5);
  scene.add(rim);
  const fill = new THREE.DirectionalLight("#ffffff", 0.35);
  fill.position.set(4, 2, 8);
  scene.add(fill);

  /* The logo, in 3D: a terminal block sitting on two slabs. */
  const logo = new THREE.Group();
  scene.add(logo);

  function roundedSlab(size, height, radius) {
    const s = size / 2 - radius;
    const shape = new THREE.Shape();
    shape.moveTo(-s, -s - radius);
    shape.lineTo(s, -s - radius);
    shape.quadraticCurveTo(s + radius, -s - radius, s + radius, -s);
    shape.lineTo(s + radius, s);
    shape.quadraticCurveTo(s + radius, s + radius, s, s + radius);
    shape.lineTo(-s, s + radius);
    shape.quadraticCurveTo(-s - radius, s + radius, -s - radius, s);
    shape.lineTo(-s - radius, -s);
    shape.quadraticCurveTo(-s - radius, -s - radius, -s, -s - radius);
    const bevel = Math.min(0.07, height * 0.3);
    const g = new THREE.ExtrudeGeometry(shape, {
      depth: height - bevel * 2, bevelEnabled: true, bevelThickness: bevel,
      bevelSize: bevel, bevelSegments: 4, curveSegments: 8,
    });
    g.rotateX(-Math.PI / 2);
    g.translate(0, bevel, 0);
    return g;
  }

  const shell = new THREE.MeshPhysicalMaterial({ metalness: 0.15, roughness: 0.5, clearcoat: 0.6, clearcoatRoughness: 0.4 });
  const edgeMat = new THREE.LineBasicMaterial({ transparent: true });

  const parts = [
    { h: 0.26, y: -1.02 },
    { h: 0.26, y: -0.62 },
    { h: 1.3, y: -0.22 },
  ];
  const slabs = parts.map(({ h, y }) => {
    const geo = roundedSlab(2.2, h, 0.26);
    const mesh = new THREE.Mesh(geo, shell);
    mesh.position.y = y;
    mesh.add(new THREE.LineSegments(new THREE.EdgesGeometry(geo, 30), edgeMat));
    logo.add(mesh);
    return { mesh, y };
  });

  // The ">_" on the front face of the top block, with a cursor that blinks.
  const faceCanvas = document.createElement("canvas");
  faceCanvas.width = 512; faceCanvas.height = 512;
  const fctx = faceCanvas.getContext("2d");
  const faceTex = new THREE.CanvasTexture(faceCanvas);
  faceTex.colorSpace = THREE.SRGBColorSpace;
  faceTex.anisotropy = 8;
  let faceInk = "#ededed";
  let cursorOn = true;
  function drawFace() {
    fctx.clearRect(0, 0, 512, 512);
    fctx.strokeStyle = faceInk;
    fctx.lineWidth = 44;
    fctx.lineCap = "round";
    fctx.lineJoin = "round";
    fctx.beginPath();
    fctx.moveTo(120, 150); fctx.lineTo(225, 240); fctx.lineTo(120, 330);
    fctx.stroke();
    if (cursorOn) { fctx.beginPath(); fctx.moveTo(285, 352); fctx.lineTo(400, 352); fctx.stroke(); }
    faceTex.needsUpdate = true;
  }
  const face = new THREE.Mesh(
    new THREE.PlaneGeometry(1.5, 1.5),
    new THREE.MeshBasicMaterial({ map: faceTex, transparent: true, toneMapped: false }),
  );
  face.position.set(0, -0.22 + 0.65, 1.1 + 0.075 + 0.004);
  face.scale.setScalar(0.78);
  logo.add(face);

  // A soft contact shadow under the stack (shows on the light page).
  const shadowTex = (() => {
    const c = document.createElement("canvas");
    c.width = c.height = 256;
    const x = c.getContext("2d");
    const g = x.createRadialGradient(128, 128, 0, 128, 128, 128);
    g.addColorStop(0, "rgba(0,0,0,1)");
    g.addColorStop(0.45, "rgba(0,0,0,0.35)");
    g.addColorStop(1, "rgba(0,0,0,0)");
    x.fillStyle = g; x.fillRect(0, 0, 256, 256);
    const t = new THREE.CanvasTexture(c);
    t.colorSpace = THREE.SRGBColorSpace;
    return t;
  })();
  const shadowMat = new THREE.MeshBasicMaterial({ map: shadowTex, transparent: true, depthWrite: false, toneMapped: false });
  const shadow = new THREE.Mesh(new THREE.PlaneGeometry(4.6, 4.6), shadowMat);
  shadow.rotation.x = -Math.PI / 2;
  shadow.position.y = -1.36;
  logo.add(shadow);

  /* The floor: a field of dots, fading into the fog. */
  const floorMat = new THREE.PointsMaterial({ size: 0.035, sizeAttenuation: true });
  {
    const pts = [];
    for (let x = -14; x <= 14; x += 0.7) {
      for (let z = -14; z <= 14; z += 0.7) {
        if (x * x + z * z < 200) pts.push(x, 0, z);
      }
    }
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(pts, 3));
    const floor = new THREE.Points(g, floorMat);
    floor.position.y = -1.6;
    scene.add(floor);
  }

  /* Destinations: Drime (Primary) and S3 (Secondary). */
  const labels = [];
  function label(text, role, weight, scale) {
    const c = document.createElement("canvas");
    c.width = 512; c.height = 128;
    const t = new THREE.CanvasTexture(c);
    t.colorSpace = THREE.SRGBColorSpace;
    t.anisotropy = 4;
    const s = new THREE.Sprite(new THREE.SpriteMaterial({ map: t, transparent: true, depthWrite: false, toneMapped: false }));
    s.scale.set(1.6 * scale, 0.4 * scale, 1);
    s.userData.base = scale;
    labels.push({ c, t, text, role, weight });
    return s;
  }
  function drawLabels(pal) {
    for (const l of labels) {
      const x = l.c.getContext("2d");
      x.clearRect(0, 0, 512, 128);
      x.font = `${l.weight} 52px Geist, system-ui, sans-serif`;
      x.fillStyle = pal[l.role];
      x.textAlign = "center";
      x.textBaseline = "middle";
      x.fillText(l.text, 256, 64);
      l.t.needsUpdate = true;
    }
  }
  const ringMats = [];
  function destination(pos, role, text, sub) {
    const g = new THREE.Group();
    g.position.copy(pos);
    const m1 = new THREE.MeshBasicMaterial({ transparent: true, opacity: 0.9, toneMapped: false });
    const m2 = new THREE.MeshBasicMaterial({ transparent: true, opacity: 0.3, toneMapped: false });
    const m3 = new THREE.MeshBasicMaterial({ toneMapped: false });
    ringMats.push({ role, mats: [m1, m2, m3] });
    const ring = new THREE.Mesh(new THREE.TorusGeometry(0.34, 0.016, 12, 72), m1);
    const ring2 = new THREE.Mesh(new THREE.TorusGeometry(0.5, 0.007, 8, 72), m2);
    const core = new THREE.Mesh(new THREE.IcosahedronGeometry(0.11, 1), m3);
    g.add(ring, ring2, core);
    const l = label(text, "ink", 600, 1); l.position.y = 0.82; g.add(l);
    const l2 = label(sub, "sub", 500, 0.8); l2.position.y = 0.58; g.add(l2);
    scene.add(g);
    return { g, ring, ring2, core };
  }
  const dests = [
    destination(new THREE.Vector3(-2.3, 1.7, 1.2), "ink", "Drime", "Primary"),
    destination(new THREE.Vector3(1.1, 1.9, -2.3), "sub", "S3 storage", "Secondary"),
  ];

  /* Files: little cards that spiral in from your folders, into r2backup,
     and out to both destinations. */
  const COUNT = window.innerWidth < 700 ? 140 : 260;
  const files = new THREE.InstancedMesh(
    new THREE.BoxGeometry(0.075, 0.095, 0.012),
    new THREE.MeshBasicMaterial({ toneMapped: false }),
    COUNT,
  );
  files.instanceMatrix.setUsage(THREE.DynamicDrawUsage);
  scene.add(files);

  const INK = new THREE.Color();
  const SUB = new THREE.Color();
  const DIM = new THREE.Color();
  const seeds = [];
  for (let i = 0; i < COUNT; i++) {
    seeds.push({
      offset: Math.random(),
      speed: 0.05 + Math.random() * 0.035,
      angle: Math.random() * Math.PI * 2,
      radius: 4.2 + Math.random() * 4.5,
      spin: (Math.random() - 0.5) * 6,
      dest: i % 2,
      lift: Math.random() * 0.8,
    });
  }
  const dummy = new THREE.Object3D();
  const color = new THREE.Color();
  const top = new THREE.Vector3(0, 1.2, 0);
  const tmp = new THREE.Vector3();
  const ctrl = new THREE.Vector3();

  function easeInOut(t) { return t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2; }

  function placeFiles(time) {
    for (let i = 0; i < COUNT; i++) {
      const s = seeds[i];
      const t = (s.offset + time * s.speed) % 1;
      let scale = 1;
      if (t < 0.56) {
        // Inbound: a narrowing spiral that ends inside the block.
        const p = t / 0.56;
        const e = easeInOut(p);
        const r = s.radius * (1 - e) + 0.15;
        const a = s.angle + e * 3.2;
        tmp.set(Math.cos(a) * r, -1.45 + s.lift * (1 - e) + e * 1.6, Math.sin(a) * r);
        color.copy(DIM).lerp(INK, e);
        scale = p > 0.92 ? (1 - p) / 0.08 : Math.min(1, p * 8);
      } else if (t < 0.64) {
        scale = 0; // inside r2backup
        tmp.set(0, 0, 0);
      } else {
        // Outbound: an arc from the top of the block to its destination.
        const p = (t - 0.64) / 0.36;
        const e = easeInOut(p);
        const d = dests[s.dest].g.position;
        ctrl.set(d.x * 0.35, 3.0 + s.lift * 0.6, d.z * 0.35);
        const u = 1 - e;
        tmp.set(
          u * u * top.x + 2 * u * e * ctrl.x + e * e * d.x,
          u * u * top.y + 2 * u * e * ctrl.y + e * e * d.y,
          u * u * top.z + 2 * u * e * ctrl.z + e * e * d.z,
        );
        color.copy(INK).lerp(s.dest ? SUB : INK, Math.min(1, e * 1.4));
        scale = p < 0.1 ? p / 0.1 : p > 0.9 ? (1 - p) / 0.1 : 1;
      }
      dummy.position.copy(tmp);
      dummy.rotation.set(time * s.spin * 0.3, time * s.spin * 0.5 + s.angle, 0);
      dummy.scale.setScalar(Math.max(scale, 0.0001));
      dummy.updateMatrix();
      files.setMatrixAt(i, dummy.matrix);
      files.setColorAt(i, color);
    }
    files.instanceMatrix.needsUpdate = true;
    if (files.instanceColor) files.instanceColor.needsUpdate = true;
  }

  /* Theme: every colour in the scene follows the page. */
  function applyTheme() {
    const pal = isDark() ? PALETTE.dark : PALETTE.light;
    scene.fog.color.set(pal.bg);
    shell.color.set(pal.shell);
    edgeMat.color.set(pal.edge);
    edgeMat.opacity = pal.edgeOpacity;
    floorMat.color.set(pal.floor);
    hemi.color.set(pal.sky);
    hemi.groundColor.set(pal.ground);
    hemi.intensity = pal.hemi;
    key.intensity = pal.key;
    rim.intensity = pal.rim;
    shadowMat.opacity = pal.shadow;
    shadow.visible = pal.shadow > 0;
    for (const r of ringMats) for (const m of r.mats) m.color.set(pal[r.role]);
    INK.set(pal.ink); SUB.set(pal.sub); DIM.set(pal.dim);
    faceInk = pal.ink;
    drawFace();
    drawLabels(pal);
  }
  applyTheme();
  if (document.fonts && document.fonts.load) {
    document.fonts.load("600 52px Geist").then(() => drawLabels(isDark() ? PALETTE.dark : PALETTE.light)).catch(() => {});
  }

  /* Layout: the band is wide on desktop and squarer on phones. */
  let w = 0, h = 0;
  function resize() {
    const r = canvas.getBoundingClientRect();
    w = Math.max(1, Math.round(r.width));
    h = Math.max(1, Math.round(r.height));
    renderer.setSize(w, h, false);
    camera.aspect = w / h;
    const aspect = w / h;
    camera.fov = aspect > 1.8 ? 25 : aspect > 1.3 ? 30 : 36;
    camera.setViewOffset(w, h, 0, h * 0.04, w, h);
    camera.updateProjectionMatrix();
    // On a phone the destination labels would be a few pixels tall; grow them.
    const grow = w < 600 ? 1.5 : 1;
    for (const d of dests) {
      for (const s of d.g.children) {
        if (!s.isSprite) continue;
        const main = s.userData.base === 1;
        s.scale.set(1.6 * s.userData.base * grow, 0.4 * s.userData.base * grow, 1);
        s.position.y = 0.5 + (main ? 0.32 : 0.08) * grow;
      }
    }
  }
  resize();
  window.addEventListener("resize", resize);

  /* Pointer parallax and scroll. */
  const pointer = { x: 0, y: 0, tx: 0, ty: 0 };
  window.addEventListener("pointermove", (e) => {
    pointer.tx = (e.clientX / window.innerWidth) * 2 - 1;
    pointer.ty = (e.clientY / window.innerHeight) * 2 - 1;
  }, { passive: true });
  let scrollP = 0;
  function onScroll() {
    const r = canvas.getBoundingClientRect();
    scrollP = Math.min(1, Math.max(0, 1 - (r.bottom / (window.innerHeight + r.height))));
  }
  window.addEventListener("scroll", onScroll, { passive: true });
  onScroll();

  const target = new THREE.Vector3(0, 0.55, 0);

  function frame(time) {
    pointer.x += (pointer.tx - pointer.x) * 0.04;
    pointer.y += (pointer.ty - pointer.y) * 0.04;

    logo.rotation.y = Math.sin(time * 0.25) * 0.12 + pointer.x * 0.25 + scrollP * 0.8;
    logo.position.y = Math.sin(time * 0.8) * 0.06;
    // the slabs breathe apart a little, like layers of a backup
    const spread = 0.04 + Math.sin(time * 0.9) * 0.03;
    slabs[0].mesh.position.y = slabs[0].y - spread * 2;
    slabs[1].mesh.position.y = slabs[1].y - spread;

    const on = Math.floor(time * 1.6) % 2 === 0;
    if (on !== cursorOn) { cursorOn = on; drawFace(); }

    for (const [i, d] of dests.entries()) {
      d.g.position.y += Math.sin(time * 0.9 + i * 2) * 0.0015;
      d.ring.rotation.x = Math.PI / 2 + Math.sin(time * 0.6 + i) * 0.3;
      d.ring.rotation.y = time * 0.5;
      d.ring2.rotation.x = Math.PI / 2 + Math.cos(time * 0.5 + i) * 0.4;
      d.ring2.rotation.z = -time * 0.3;
      d.core.scale.setScalar(1 + Math.sin(time * 2 + i * 1.7) * 0.12);
      d.g.quaternion.copy(camera.quaternion);
    }

    placeFiles(time);

    camera.position.set(
      camBase.x - pointer.x * 0.6,
      camBase.y + pointer.y * 0.35 + scrollP * 1.6,
      camBase.z,
    );
    camera.lookAt(target);
    renderer.render(scene, camera);
  }

  if (reduceMotion) {
    const still = () => frame(4.2);
    still();
    window.addEventListener("resize", still);
    window.addEventListener("r2b-theme", () => { applyTheme(); still(); });
    return;
  }
  window.addEventListener("r2b-theme", applyTheme);

  let running = true;
  new IntersectionObserver(([e]) => { running = e.isIntersecting; }, { threshold: 0 }).observe(canvas);
  const clock = new THREE.Clock();
  let elapsed = 0;
  renderer.setAnimationLoop(() => {
    const dt = Math.min(clock.getDelta(), 0.05);
    if (!running || document.hidden) return;
    elapsed += dt;
    frame(elapsed);
  });
}

const canvas = $("scene");
try {
  startScene(canvas);
} catch (err) {
  // No WebGL (old GPU driver, locked-down browser): the grid stays and the page reads fine.
  canvas.remove();
  console.warn("3D scene unavailable:", err);
}

/* ------------------------------------------------------ release details -- */

function platform() {
  const ua = navigator.userAgent;
  const p = (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || "";
  if (/win/i.test(p) || /Windows/.test(ua)) return "windows";
  if (/mac/i.test(p) || /Mac OS X/.test(ua)) return "mac";
  if (/linux|x11|cros/i.test(p + ua) && !/Android/.test(ua)) return "linux";
  return "";
}

function heroButton() {
  const os = platform();
  const label = $("cta-label");
  if (os === "windows") {
    label.textContent = "Download for Windows";
    $("cta-main").href = "/download/latest/r2backup-setup.exe";
    $("cta-close").href = "/download/latest/r2backup-setup.exe";
    $("cta-fine").textContent = "Installs for your account, no administrator prompt";
  } else if (os === "mac") {
    label.textContent = "Install on macOS";
  } else if (os === "linux") {
    label.textContent = "Install on Linux";
  }
}
heroButton();

function escapeHTML(s) {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// Just enough Markdown for release notes: headings, bullets, paragraphs,
// **bold**, `code` and [links](https://...). Escaped first, so nothing in the
// notes can become markup it was not meant to be.
function renderNotes(md) {
  const inline = (s) => escapeHTML(s)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/\[([^\]]+)\]\((https:\/\/[^)\s]+)\)/g, '<a href="$2" rel="noopener">$1</a>');
  const out = [];
  let list = false;
  for (const raw of md.replace(/\r/g, "").split("\n")) {
    const line = raw.trimEnd();
    const bullet = line.match(/^\s*[-*]\s+(.*)$/);
    if (bullet) {
      if (!list) { out.push("<ul>"); list = true; }
      out.push(`<li>${inline(bullet[1])}</li>`);
      continue;
    }
    if (list && /^\s{2,}\S/.test(line)) { out[out.length - 1] = out[out.length - 1].replace(/<\/li>$/, " " + inline(line.trim()) + "</li>"); continue; }
    if (list) { out.push("</ul>"); list = false; }
    const head = line.match(/^#{1,6}\s+(.*)$/);
    if (head) out.push(`<h4>${inline(head[1])}</h4>`);
    else if (line.trim()) out.push(`<p>${inline(line)}</p>`);
  }
  if (list) out.push("</ul>");
  return out.join("");
}

async function release() {
  try {
    const res = await fetch("/api/release/latest.json", { cache: "no-cache" });
    if (!res.ok) return;
    const rel = await res.json();
    const tag = rel.tag_name;
    const date = rel.published_at ? new Date(rel.published_at).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" }) : "";
    $("ver-hero").textContent = tag;
    const ver = document.createElement("span");
    ver.className = "num";
    ver.textContent = tag;
    $("dl-version").replaceChildren(ver, date ? ` · released ${date}` : "");

    const byName = new Map(rel.assets.map((a) => [a.name, a]));
    document.querySelectorAll("[data-zip]").forEach((a) => {
      const k = a.dataset.zip;
      const hit = rel.assets.find((x) => x.name.includes(`_${k}.`) && /\.(zip|tar\.gz)$/.test(x.name));
      if (hit) {
        a.href = hit.browser_download_url;
        a.title = `${hit.name} · ${(hit.size / 1048576).toFixed(1)} MB`;
      } else a.remove();
    });
    document.querySelectorAll("[data-asset]").forEach((a) => {
      const hit = byName.get(a.dataset.asset);
      if (hit) a.title = `${hit.name} · ${(hit.size / 1048576).toFixed(1)} MB`;
    });
    if (rel.body && rel.body.trim()) {
      $("notes-ver").textContent = tag;
      $("notes-body").innerHTML = renderNotes(rel.body);
      $("notes").hidden = false;
    }
  } catch { /* the page still works with the /download/latest links */ }
}
release();

/* ------------------------------------------------------------ niceties -- */

document.querySelectorAll(".copy").forEach((b) => {
  b.addEventListener("click", async () => {
    const text = $(b.dataset.copy).textContent;
    try {
      await navigator.clipboard.writeText(text);
      b.classList.add("done");
      $("live").textContent = "Copied to the clipboard";
      setTimeout(() => { b.classList.remove("done"); $("live").textContent = ""; }, 1600);
    } catch {
      const r = document.createRange(); r.selectNodeContents($(b.dataset.copy));
      const s = getSelection(); s.removeAllRanges(); s.addRange(r);
    }
  });
});

// The benchmark bars draw in once, the first time the table is on screen.
const bench = document.querySelector(".bench");
if (bench && !reduceMotion && "IntersectionObserver" in window) {
  const r = bench.getBoundingClientRect();
  if (r.top > window.innerHeight) {
    bench.classList.add("wait");
    const io = new IntersectionObserver(([e]) => {
      if (e.isIntersecting) { bench.classList.remove("wait"); io.disconnect(); }
    }, { rootMargin: "0px 0px -15% 0px" });
    io.observe(bench);
  }
}
