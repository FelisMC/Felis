import { useEffect, useRef } from "react";
import * as THREE from "three";
import { phaseColor } from "@/components/PhaseBadge";
import type { ServerInfo } from "@/lib/types";

// VoxelFleet renders the fleet as a grid of voxels, one per server, colored by
// lifecycle phase — the 3D view of the same state machine the list shows (spec
// §22 phases). Design constraints (panel/DESIGN.md §"where 3D earns its place"):
//   • exactly ONE WebGLRenderer for the whole app surface;
//   • the render loop is paused while the tab is hidden (no background GPU burn);
//   • everything is disposed on unmount (no context leak on route changes).
// Raw three.js (not react-three-fiber) keeps the dependency/build surface minimal.

interface Props {
  servers: ServerInfo[];
}

const STARTING = "Starting";

export function VoxelFleet({ servers }: Props) {
  const mountRef = useRef<HTMLDivElement>(null);
  // Latest servers without re-running the heavy setup effect on every poll.
  const serversRef = useRef(servers);
  serversRef.current = servers;

  useEffect(() => {
    const mount = mountRef.current;
    if (!mount) return;

    const scene = new THREE.Scene();
    const camera = new THREE.PerspectiveCamera(45, 1, 0.1, 100);
    camera.position.set(6, 5.5, 9);
    camera.lookAt(0, 0, 0);

    const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true });
    renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
    mount.appendChild(renderer.domElement);
    renderer.domElement.style.display = "block";

    scene.add(new THREE.AmbientLight(0xffffff, 0.55));
    const key = new THREE.DirectionalLight(0xffffff, 1.1);
    key.position.set(5, 8, 6);
    scene.add(key);
    const fill = new THREE.DirectionalLight(0x6699ff, 0.35);
    fill.position.set(-6, 2, -4);
    scene.add(fill);

    const group = new THREE.Group();
    scene.add(group);

    const geometry = new THREE.BoxGeometry(0.9, 0.9, 0.9);
    // Tracks the live meshes so we can dispose materials and rebuild on change.
    let voxels: { mesh: THREE.Mesh; pulsing: boolean; base: number }[] = [];

    function clearVoxels() {
      for (const v of voxels) {
        group.remove(v.mesh);
        (v.mesh.material as THREE.Material).dispose();
      }
      voxels = [];
    }

    function build(list: ServerInfo[]) {
      clearVoxels();
      const n = Math.max(list.length, 1);
      const cols = Math.ceil(Math.sqrt(n));
      const rows = Math.ceil(n / cols);
      const spacing = 1.25;
      const offX = ((cols - 1) * spacing) / 2;
      const offZ = ((rows - 1) * spacing) / 2;

      list.forEach((s, i) => {
        const color = new THREE.Color(phaseColor(s.phase));
        const material = new THREE.MeshStandardMaterial({
          color,
          roughness: 0.45,
          metalness: 0.1,
          emissive: color.clone().multiplyScalar(s.phase === "Running" ? 0.25 : 0.05),
        });
        const mesh = new THREE.Mesh(geometry, material);
        const c = i % cols;
        const r = Math.floor(i / cols);
        mesh.position.set(c * spacing - offX, 0, r * spacing - offZ);
        const base = s.phase === STARTING ? 0.5 : 0.9;
        mesh.scale.setScalar(base);
        group.add(mesh);
        voxels.push({ mesh, pulsing: s.phase === STARTING, base });
      });
    }

    build(serversRef.current);

    // Rebuild only when the *shape* (names + phases) of the fleet changes, polled
    // cheaply against a signature so we avoid teardown churn on identical data.
    let signature = sig(serversRef.current);
    const rebuildTimer = window.setInterval(() => {
      const next = sig(serversRef.current);
      if (next !== signature) {
        signature = next;
        build(serversRef.current);
      }
    }, 1000);

    function resize() {
      const w = mount!.clientWidth || 1;
      const h = mount!.clientHeight || 1;
      renderer.setSize(w, h, false);
      camera.aspect = w / h;
      camera.updateProjectionMatrix();
    }
    resize();
    const ro = new ResizeObserver(resize);
    ro.observe(mount);

    let raf = 0;
    let t = 0;
    let running = true;
    const tick = () => {
      if (!running) return;
      raf = requestAnimationFrame(tick);
      t += 0.016;
      group.rotation.y = t * 0.18;
      for (const v of voxels) {
        if (v.pulsing) {
          const s = v.base + Math.sin(t * 4) * 0.12 + 0.12;
          v.mesh.scale.setScalar(s);
        }
      }
      renderer.render(scene, camera);
    };
    tick();

    // Pause the loop while the tab is hidden — no GPU work in the background.
    const onVisibility = () => {
      if (document.hidden) {
        running = false;
        cancelAnimationFrame(raf);
      } else if (!running) {
        running = true;
        tick();
      }
    };
    document.addEventListener("visibilitychange", onVisibility);

    return () => {
      running = false;
      cancelAnimationFrame(raf);
      window.clearInterval(rebuildTimer);
      document.removeEventListener("visibilitychange", onVisibility);
      ro.disconnect();
      clearVoxels();
      geometry.dispose();
      renderer.dispose();
      if (renderer.domElement.parentNode === mount) {
        mount.removeChild(renderer.domElement);
      }
    };
  }, []);

  return <div ref={mountRef} className="h-full w-full" />;
}

/** sig is a cheap fingerprint of the fleet's renderable shape (order-independent
 *  per index): name+phase pairs. Player counts don't change the voxels, so they
 *  don't trigger a rebuild. */
function sig(servers: ServerInfo[]): string {
  return servers.map((s) => `${s.name}:${s.phase}`).join("|");
}
