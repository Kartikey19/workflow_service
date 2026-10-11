import { useMemo } from "react";
import { ReactFlow, Background, Controls, MarkerType } from "@xyflow/react";
import dagre from "@dagrejs/dagre";
import type { RecordData } from "./api";
import { Empty } from "./components";
import "@xyflow/react/dist/style.css";
export function Graph({ spec }: { spec: RecordData | null }) {
  const { nodes, edges } = useMemo(() => {
    const g = new dagre.graphlib.Graph().setDefaultEdgeLabel(() => ({}));
    g.setGraph({ rankdir: "LR", nodesep: 32, ranksep: 64 });
    const source: RecordData[] = spec?.nodes || [];
    source.forEach((n) => g.setNode(n.id, { width: 180, height: 70 }));
    const edges = source.flatMap((n) =>
      (n.needs || []).map((dependency: string) => ({
        id: `${dependency}-${n.id}`,
        source: dependency,
        target: n.id,
        markerEnd: { type: MarkerType.ArrowClosed },
      })),
    );
    edges.forEach((e) => g.setEdge(e.source, e.target));
    dagre.layout(g);
    return {
      edges,
      nodes: source.map((n) => {
        const p = g.node(n.id);
        return {
          id: n.id,
          position: { x: p.x - 90, y: p.y - 35 },
          data: {
            label: (
              <>
                <strong>{n.id}</strong>
                <small>{n.type}</small>
              </>
            ),
          },
          style: { width: 180, height: 70 },
        };
      }),
    };
  }, [spec]);
  return (
    <div className="graph" aria-label="Workflow dependency graph">
      {nodes.length ? (
        <ReactFlow
          key={nodes.map((n) => n.id).join()}
          nodes={nodes}
          edges={edges}
          fitView
          nodesDraggable={false}
          nodesConnectable={false}
          edgesFocusable={false}
        >
          <Background color="#d9e2dc" />
          <Controls showInteractive={false} />
        </ReactFlow>
      ) : (
        <Empty>Validate a workflow to see its graph.</Empty>
      )}
    </div>
  );
}
