"use client";

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import ForceGraph2D, {
  type ForceGraphMethods,
  type NodeObject,
} from "react-force-graph-2d";
import type { ConceptGraph as GraphData, GraphNode } from "@/lib/api";

/**
 * Node radius from connection count.
 *
 * Logarithmic: a concept with 40 connections matters more than one with 4, but
 * not ten times the area — linear scaling lets a single hub swamp the canvas.
 * Kept small so the graph reads as a constellation of labelled points rather
 * than a cluster of discs.
 */
const BASE_RADIUS = 3;
const RADIUS_SCALE = 1.8;

function radiusFor(connections: number): number {
  return BASE_RADIUS + RADIUS_SCALE * Math.log1p(connections);
}

/**
 * Labels are drawn at a constant *screen* size, so they stay readable at any
 * zoom. Sizing them in graph units instead made them shrink with the view: at
 * the zoom `zoomToFit` chooses for a 150-node graph they rendered sub-pixel and
 * then faded out entirely, leaving a canvas of anonymous dots.
 *
 * The fade only guards the far-zoomed-out case, where hundreds of overlapping
 * labels would be noise rather than information.
 */
const LABEL_SCREEN_PX = 11;
const LABEL_FADE_START = 0.12;
const LABEL_FADE_FULL = 0.25;

/**
 * Labels are revealed progressively by connection count as the view zooms in.
 *
 * Painting all 150 at the zoom that fits the whole graph turns the dense core
 * into overlapping mush — the text is legible individually and unreadable
 * collectively. Showing only hubs when zoomed out keeps the overview oriented
 * ("this region is about solid-state batteries") and hands over the detail once
 * the viewer has actually zoomed into a region and can see it.
 */
const LABEL_ALWAYS_ABOVE = 8;
const LABEL_REVEAL_ZOOM = 2.2;

function labelThresholdFor(scale: number): number {
  if (scale >= LABEL_REVEAL_ZOOM) return 0;
  const t = clamp(scale / LABEL_REVEAL_ZOOM, 0, 1);
  return Math.round(LABEL_ALWAYS_ABOVE * (1 - t));
}

/** Label wrapping, so long concept names stack instead of overlapping. */
const LABEL_MAX_CHARS_PER_LINE = 20;
const LABEL_MAX_LINES = 2;

interface Palette {
  background: string;
  node: string;
  nodeSelected: string;
  nodeDimmed: string;
  link: string;
  linkActive: string;
  label: string;
  labelDimmed: string;
}

// Nodes are one uniform colour: importance is carried by size and by how many
// lines converge on a point. Tinting by degree as well made every reading of
// the graph redundant and noisy.
const LIGHT: Palette = {
  background: "#fafafa",
  node: "#8c8c8c",
  nodeSelected: "#4a4a4a",
  nodeDimmed: "#d8d8d8",
  link: "rgba(0,0,0,0.10)",
  linkActive: "rgba(0,0,0,0.38)",
  label: "#6b6b6b",
  labelDimmed: "rgba(107,107,107,0.22)",
};

const DARK: Palette = {
  background: "#1a1a1a",
  node: "#b4b4b4",
  nodeSelected: "#ffffff",
  nodeDimmed: "#3a3a3a",
  link: "rgba(255,255,255,0.09)",
  linkActive: "rgba(255,255,255,0.34)",
  label: "#9a9a9a",
  labelDimmed: "rgba(154,154,154,0.22)",
};

/**
 * Tracks the viewer's colour scheme so canvas painting matches the page.
 *
 * A media query is an external store, so useSyncExternalStore is the correct
 * hook: it reads the current value during render instead of setting state from
 * an effect, which would paint the wrong palette for one frame.
 */
function usePalette(): Palette {
  const isDark = useSyncExternalStore(
    subscribeToColorScheme,
    getColorSchemeSnapshot,
    // Server render has no matchMedia; light is the safe default.
    () => false,
  );
  return isDark ? DARK : LIGHT;
}

function subscribeToColorScheme(onChange: () => void): () => void {
  const query = window.matchMedia("(prefers-color-scheme: dark)");
  query.addEventListener("change", onChange);
  return () => query.removeEventListener("change", onChange);
}

function getColorSchemeSnapshot(): boolean {
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

/** Node as the force simulation augments it — coordinates added at runtime. */
type SimNode = NodeObject & GraphNode;

interface ConceptGraphProps {
  graph: GraphData;
  selectedId: string | null;
  onSelect: (node: GraphNode | null) => void;
}

export function ConceptGraph({ graph, selectedId, onSelect }: ConceptGraphProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const graphRef = useRef<ForceGraphMethods<SimNode, GraphEdgeObject> | undefined>(undefined);
  const [size, setSize] = useState({ width: 0, height: 0 });
  const [hoveredId, setHoveredId] = useState<string | null>(null);
  const palette = usePalette();

  // The canvas needs explicit pixel dimensions, so track the container.
  useEffect(() => {
    const element = containerRef.current;
    if (!element) return;

    const observer = new ResizeObserver(([entry]) => {
      const { width, height } = entry.contentRect;
      setSize({ width, height });
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  // react-force-graph mutates the objects it is given, which would corrupt the
  // caller's state across re-renders. Hand it a fresh copy.
  const data = useMemo(
    () => ({
      nodes: graph.nodes.map((n) => ({ ...n })),
      links: graph.edges.map((e) => ({ ...e })),
    }),
    [graph],
  );

  /** Ids one hop from the active node, used to dim everything unrelated. */
  const neighbourIds = useMemo(() => {
    const active = hoveredId ?? selectedId;
    if (!active) return null;

    const ids = new Set<string>([active]);
    for (const edge of graph.edges) {
      if (edge.source === active) ids.add(edge.target);
      else if (edge.target === active) ids.add(edge.source);
    }
    return ids;
  }, [graph.edges, hoveredId, selectedId]);

  const paintNode = useCallback(
    (node: SimNode, ctx: CanvasRenderingContext2D, scale: number) => {
      if (node.x === undefined || node.y === undefined) return;

      const radius = radiusFor(node.connection_count);
      const isSelected = node.id === selectedId;
      const isActive = isSelected || node.id === hoveredId;
      const isRelevant = !neighbourIds || neighbourIds.has(node.id as string);

      ctx.beginPath();
      ctx.arc(node.x, node.y, radius, 0, 2 * Math.PI);
      ctx.fillStyle = !isRelevant
        ? palette.nodeDimmed
        : isActive
          ? palette.nodeSelected
          : palette.node;
      ctx.fill();

      if (isSelected) {
        ctx.strokeStyle = palette.nodeSelected;
        ctx.lineWidth = 0.6;
        ctx.beginPath();
        ctx.arc(node.x, node.y, radius + 2.5, 0, 2 * Math.PI);
        ctx.stroke();
      }

      // The active node and its neighbours always keep their labels — hovering a
      // node to read what it connects to is the graph's main interaction, and
      // hiding those names would defeat it.
      const isFocus = isActive || (neighbourIds !== null && isRelevant);
      if (!isFocus && node.connection_count < labelThresholdFor(scale)) return;

      // Labels fade with zoom rather than vanishing abruptly, so pulling back
      // for an overview does not snap the whole canvas to anonymous dots.
      const zoomAlpha = isActive
        ? 1
        : clamp((scale - LABEL_FADE_START) / (LABEL_FADE_FULL - LABEL_FADE_START), 0, 1);
      if (zoomAlpha <= 0.02) return;

      // Dividing by the zoom scale keeps the rendered text at a fixed pixel
      // size on screen, independent of how far the view is zoomed out.
      const fontSize = LABEL_SCREEN_PX / scale;
      const lines = wrapLabel(node.name);

      ctx.font = `${fontSize}px ui-sans-serif, system-ui, -apple-system, sans-serif`;
      ctx.textAlign = "center";
      ctx.textBaseline = "top";

      const previousAlpha = ctx.globalAlpha;
      ctx.globalAlpha = previousAlpha * zoomAlpha;
      ctx.fillStyle = isRelevant ? palette.label : palette.labelDimmed;

      const lineHeight = fontSize * 1.1;
      let y = node.y + radius + fontSize * 0.45;
      for (const line of lines) {
        ctx.fillText(line, node.x, y);
        y += lineHeight;
      }
      ctx.globalAlpha = previousAlpha;
    },
    [hoveredId, neighbourIds, palette, selectedId],
  );

  const linkColor = useCallback(
    (link: GraphEdgeObject) => {
      const active = hoveredId ?? selectedId;
      if (!active) return palette.link;

      const source = idOf(link.source);
      const target = idOf(link.target);
      return source === active || target === active ? palette.linkActive : palette.link;
    },
    [hoveredId, palette, selectedId],
  );

  // Spread the layout out. The library's defaults pack nodes tightly, which
  // buries labels under one another; a knowledge graph is mostly unreadable
  // without them, so the simulation is tuned for whitespace over density.
  //
  // hasCanvas is a dependency, not decoration: the ref is only populated once
  // ForceGraph2D mounts, which happens after the ResizeObserver reports a
  // size. Keying this on data alone runs it while the ref is still null and
  // the tuning silently never applies.
  const hasCanvas = size.width > 0;

  useEffect(() => {
    const graph = graphRef.current;
    if (!graph) return;

    graph.d3Force("charge")?.strength(-420).distanceMax(600);
    graph.d3Force("link")?.distance(110).strength(0.3);
    graph.d3Force("center")?.strength(0.04);
    graph.d3ReheatSimulation();
  }, [data, hasCanvas]);

  // Recentre once the layout has settled, so a freshly loaded graph is neither
  // off-screen nor framed mid-explosion.
  useEffect(() => {
    if (!hasCanvas) return;
    const timer = setTimeout(() => graphRef.current?.zoomToFit(600, 40), 900);
    return () => clearTimeout(timer);
  }, [data, hasCanvas]);

  return (
    <div ref={containerRef} className="h-full w-full">
      {size.width > 0 && (
        <ForceGraph2D
          ref={graphRef}
          width={size.width}
          height={size.height}
          graphData={data}
          backgroundColor={palette.background}
          nodeCanvasObject={paintNode}
          nodePointerAreaPaint={(node, color, ctx) => {
            const n = node as SimNode;
            if (n.x === undefined || n.y === undefined) return;
            ctx.fillStyle = color;
            ctx.beginPath();
            // Slightly larger than the drawn node so small ones stay clickable.
            ctx.arc(n.x, n.y, radiusFor(n.connection_count) + 3, 0, 2 * Math.PI);
            ctx.fill();
          }}
          linkColor={linkColor}
          linkWidth={0.4}
          onNodeClick={(node) => onSelect(node as SimNode)}
          onNodeHover={(node) => setHoveredId(node ? ((node as SimNode).id as string) : null)}
          onBackgroundClick={() => onSelect(null)}
          cooldownTicks={300}
          d3VelocityDecay={0.25}
          nodeRelSize={1}
        />
      )}
    </div>
  );
}

/** Links carry either an id or the resolved node once the simulation runs. */
interface GraphEdgeObject {
  source: string | SimNode;
  target: string | SimNode;
  summary?: string;
}

function idOf(end: string | SimNode): string {
  return typeof end === "string" ? end : (end.id as string);
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value));
}

/**
 * Breaks a concept name onto short lines so long names stack under their node
 * instead of overlapping neighbouring labels.
 *
 * Memoised because this runs for every node on every animation frame while the
 * simulation settles, and the result only depends on the name.
 */
const wrapCache = new Map<string, string[]>();

function wrapLabel(name: string): string[] {
  const cached = wrapCache.get(name);
  if (cached) return cached;

  const words = name.split(/\s+/);
  const lines: string[] = [];
  let current = "";

  for (const word of words) {
    const candidate = current ? `${current} ${word}` : word;
    if (candidate.length <= LABEL_MAX_CHARS_PER_LINE || current === "") {
      current = candidate;
      continue;
    }
    lines.push(current);
    current = word;
    if (lines.length === LABEL_MAX_LINES) break;
  }

  if (current && lines.length < LABEL_MAX_LINES) lines.push(current);
  if (lines.length === LABEL_MAX_LINES && words.join(" ").length > lines.join(" ").length) {
    lines[LABEL_MAX_LINES - 1] = `${lines[LABEL_MAX_LINES - 1]}…`;
  }

  wrapCache.set(name, lines);
  return lines;
}
