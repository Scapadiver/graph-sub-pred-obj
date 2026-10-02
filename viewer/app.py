#!/usr/bin/env python3
"""
Interactive RDF Property Graph Viewer.

Connects to Aerospike and provides a web-based interactive graph visualization
with double-click expansion, predicate filtering, hop limits, and property inspection.

Usage:
    python app.py --host 127.0.0.1 --port 3000 --namespace test
"""

import argparse
import hashlib
import json

import aerospike
import dash
from dash import html, dcc, callback_context
from dash.dependencies import Input, Output, State
import dash_cytoscape as cyto

# ---------------------------------------------------------------------------
# Aerospike helpers
# ---------------------------------------------------------------------------

SETNAME = "triples"


def connect_aerospike(host, port, namespace):
    config = {"hosts": [(host, port)]}
    client = aerospike.client(config).connect()
    return client, namespace


def triple_key(s, p, o):
    h = hashlib.sha256()
    h.update(s.encode())
    h.update(b"\x00")
    h.update(p.encode())
    h.update(b"\x00")
    h.update(o.encode())
    return h.hexdigest()


def query_by_bin(client, namespace, bin_name, value):
    """Secondary index query on a single bin."""
    query = client.query(namespace, SETNAME)
    query.where(aerospike.predicates.equals(bin_name, value))
    results = []
    for _, _, bins in query.results():
        results.append(bins)
    return results


def query_sp(client, namespace, subject, predicate):
    """SP? pattern: subject + predicate filter."""
    triples = query_by_bin(client, namespace, "subject", subject)
    return [t for t in triples if t.get("predicate") == predicate]


def query_po(client, namespace, predicate, obj):
    """?PO pattern: predicate + object filter."""
    triples = query_by_bin(client, namespace, "predicate", predicate)
    return [t for t in triples if t.get("object") == obj]


def get_all_predicates(client, namespace):
    """Scan to discover all unique predicates."""
    scan = client.scan(namespace, SETNAME)
    scan.select("predicate")
    predicates = set()
    for _, _, bins in scan.results():
        predicates.add(bins.get("predicate", ""))
    return sorted(predicates)


def get_all_nodes(client, namespace):
    """Scan to discover all unique subjects and objects."""
    scan = client.scan(namespace, SETNAME)
    scan.select("subject", "object")
    nodes = set()
    for _, _, bins in scan.results():
        nodes.add(bins.get("subject", ""))
        nodes.add(bins.get("object", ""))
    return sorted(nodes)


def expand_node(client, namespace, node, predicates=None, direction="both"):
    """Get all triples connected to a node, optionally filtered by predicates and direction."""
    triples = []
    if direction in ("both", "outbound"):
        outbound = query_by_bin(client, namespace, "subject", node)
        triples.extend(outbound)
    if direction in ("both", "inbound"):
        inbound = query_by_bin(client, namespace, "object", node)
        triples.extend(inbound)

    if predicates:
        triples = [t for t in triples if t.get("predicate") in predicates]

    return triples


def bfs_expand(client, namespace, start_node, max_hops, predicates=None, direction="both"):
    """BFS traversal from start_node up to max_hops."""
    visited_nodes = {start_node}
    visited_edges = set()
    all_triples = []
    current_level = [start_node]

    for _ in range(max_hops):
        if not current_level:
            break
        next_level = []
        for node in current_level:
            triples = expand_node(client, namespace, node, predicates, direction)
            for t in triples:
                s, p, o = t["subject"], t["predicate"], t["object"]
                edge_key = (s, p, o)
                if edge_key not in visited_edges:
                    visited_edges.add(edge_key)
                    all_triples.append(t)
                    for n in (s, o):
                        if n not in visited_nodes:
                            visited_nodes.add(n)
                            next_level.append(n)
        current_level = next_level

    return all_triples


# ---------------------------------------------------------------------------
# Graph element builders
# ---------------------------------------------------------------------------

NODE_COLORS = {
    "user": "#4FC3F7",
    "post": "#81C784",
    "topic": "#FFB74D",
    "default": "#B0BEC5",
}


def node_type(node_id):
    if ":" in node_id:
        return node_id.split(":")[0]
    return "default"


def build_elements(triples):
    """Convert triples to Cytoscape elements (nodes + edges)."""
    nodes = {}
    edges = []

    for t in triples:
        s, p, o = t["subject"], t["predicate"], t["object"]
        props = t.get("props", {})
        if isinstance(props, bytes):
            props = {}

        for nid in (s, o):
            if nid not in nodes:
                nt = node_type(nid)
                nodes[nid] = {
                    "data": {
                        "id": nid,
                        "label": nid,
                        "type": nt,
                        "color": NODE_COLORS.get(nt, NODE_COLORS["default"]),
                    }
                }

        edge_id = f"{s}|{p}|{o}"
        props_str = json.dumps(props, default=str) if props else "{}"
        edges.append({
            "data": {
                "id": edge_id,
                "source": s,
                "target": o,
                "label": p,
                "props": props_str,
            }
        })

    return list(nodes.values()) + edges


# ---------------------------------------------------------------------------
# Dash app
# ---------------------------------------------------------------------------

def create_app(client, namespace):
    app = dash.Dash(__name__)

    all_predicates = get_all_predicates(client, namespace)
    all_nodes = get_all_nodes(client, namespace)

    cyto.load_extra_layouts()

    app.layout = html.Div([
        # Header
        html.Div([
            html.H2("RDF Property Graph Viewer",
                     style={"margin": "0", "color": "#fff"}),
            html.Span("Double-click a node to expand its relationships",
                       style={"color": "#B0BEC5", "fontSize": "14px"}),
        ], style={
            "background": "#263238", "padding": "16px 24px",
            "display": "flex", "justifyContent": "space-between", "alignItems": "center",
        }),

        html.Div([
            # Left panel - controls
            html.Div([
                html.H4("Controls", style={"marginTop": "0"}),

                html.Label("Start Node"),
                dcc.Dropdown(
                    id="start-node",
                    options=[{"label": n, "value": n} for n in all_nodes],
                    placeholder="Select or type a node...",
                    searchable=True,
                    style={"marginBottom": "12px"},
                ),

                html.Label("Max Hops"),
                dcc.Slider(
                    id="max-hops",
                    min=1, max=6, step=1, value=2,
                    marks={i: str(i) for i in range(1, 7)},
                ),

                html.Label("Predicate Filter", style={"marginTop": "12px"}),
                dcc.Dropdown(
                    id="predicate-filter",
                    options=[{"label": p, "value": p} for p in all_predicates],
                    multi=True,
                    placeholder="All predicates",
                    style={"marginBottom": "12px"},
                ),

                html.Label("Direction"),
                dcc.RadioItems(
                    id="direction",
                    options=[
                        {"label": "Both", "value": "both"},
                        {"label": "Outbound", "value": "outbound"},
                        {"label": "Inbound", "value": "inbound"},
                    ],
                    value="both",
                    style={"marginBottom": "12px"},
                ),

                html.Label("Layout"),
                dcc.Dropdown(
                    id="layout-select",
                    options=[
                        {"label": "Cola (force-directed)", "value": "cola"},
                        {"label": "Dagre (hierarchical)", "value": "dagre"},
                        {"label": "Breadthfirst", "value": "breadthfirst"},
                        {"label": "Circle", "value": "circle"},
                        {"label": "Concentric", "value": "concentric"},
                        {"label": "Grid", "value": "grid"},
                    ],
                    value="cola",
                    clearable=False,
                    style={"marginBottom": "16px"},
                ),

                html.Button("Explore", id="explore-btn", n_clicks=0,
                             style={
                                 "width": "100%", "padding": "10px",
                                 "background": "#4FC3F7", "color": "#fff",
                                 "border": "none", "borderRadius": "4px",
                                 "cursor": "pointer", "fontSize": "16px",
                                 "marginBottom": "16px",
                             }),

                html.Button("Clear Graph", id="clear-btn", n_clicks=0,
                             style={
                                 "width": "100%", "padding": "10px",
                                 "background": "#EF5350", "color": "#fff",
                                 "border": "none", "borderRadius": "4px",
                                 "cursor": "pointer", "fontSize": "14px",
                                 "marginBottom": "16px",
                             }),

                html.Hr(),

                # Node/Edge info panel
                html.H4("Selected Element"),
                html.Div(id="info-panel",
                         children="Click a node or edge to see details.",
                         style={
                             "background": "#f5f5f5", "padding": "12px",
                             "borderRadius": "4px", "fontSize": "13px",
                             "whiteSpace": "pre-wrap", "maxHeight": "300px",
                             "overflowY": "auto",
                         }),

                html.Hr(),

                # Stats
                html.H4("Graph Stats"),
                html.Div(id="stats-panel", style={"fontSize": "13px"}),

            ], style={
                "width": "300px", "padding": "16px",
                "borderRight": "1px solid #e0e0e0",
                "overflowY": "auto", "height": "calc(100vh - 60px)",
            }),

            # Right panel - graph
            html.Div([
                cyto.Cytoscape(
                    id="graph",
                    elements=[],
                    layout={"name": "cola", "animate": True, "maxSimulationTime": 2000},
                    style={"width": "100%", "height": "calc(100vh - 60px)"},
                    stylesheet=[
                        # Node styles
                        {
                            "selector": "node",
                            "style": {
                                "label": "data(label)",
                                "background-color": "data(color)",
                                "color": "#263238",
                                "font-size": "12px",
                                "text-valign": "bottom",
                                "text-margin-y": "8px",
                                "width": "40px",
                                "height": "40px",
                                "border-width": "2px",
                                "border-color": "#455A64",
                            },
                        },
                        # Expanded node highlight
                        {
                            "selector": "node:selected",
                            "style": {
                                "border-width": "4px",
                                "border-color": "#FF7043",
                                "width": "50px",
                                "height": "50px",
                            },
                        },
                        # Edge styles
                        {
                            "selector": "edge",
                            "style": {
                                "label": "data(label)",
                                "curve-style": "bezier",
                                "target-arrow-shape": "triangle",
                                "target-arrow-color": "#78909C",
                                "line-color": "#B0BEC5",
                                "font-size": "10px",
                                "color": "#546E7A",
                                "text-rotation": "autorotate",
                                "text-margin-y": "-10px",
                                "width": 2,
                                "arrow-scale": 1.2,
                            },
                        },
                        {
                            "selector": "edge:selected",
                            "style": {
                                "line-color": "#FF7043",
                                "target-arrow-color": "#FF7043",
                                "width": 3,
                            },
                        },
                    ],
                ),
            ], style={"flex": "1"}),
        ], style={"display": "flex", "height": "calc(100vh - 60px)"}),

        # Hidden stores
        dcc.Store(id="graph-data", data={"elements": [], "expanded": []}),

    ], style={"fontFamily": "system-ui, sans-serif", "margin": "0"})

    # --- Callbacks ---

    @app.callback(
        Output("graph-data", "data"),
        [Input("explore-btn", "n_clicks"),
         Input("clear-btn", "n_clicks"),
         Input("graph", "tapNodeData")],
        [State("start-node", "value"),
         State("max-hops", "value"),
         State("predicate-filter", "value"),
         State("direction", "value"),
         State("graph-data", "data"),
         State("graph", "tapNode")],
        prevent_initial_call=True,
    )
    def update_graph_data(explore_clicks, clear_clicks, tap_data,
                          start_node, max_hops, pred_filter, direction,
                          current_data, tap_node):
        ctx = callback_context
        if not ctx.triggered:
            return current_data

        trigger_id = ctx.triggered[0]["prop_id"].split(".")[0]

        if trigger_id == "clear-btn":
            return {"elements": [], "expanded": []}

        if trigger_id == "explore-btn":
            if not start_node:
                return current_data
            preds = pred_filter if pred_filter else None
            triples = bfs_expand(client, namespace, start_node, max_hops, preds, direction)
            elements = build_elements(triples)
            return {"elements": elements, "expanded": [start_node]}

        if trigger_id == "graph" and tap_data:
            # Double-click detection via tap - expand the node
            node_id = tap_data["id"]
            expanded = current_data.get("expanded", [])

            if node_id in expanded:
                return current_data

            expanded.append(node_id)
            preds = pred_filter if pred_filter else None
            new_triples = expand_node(client, namespace, node_id, preds, direction)
            new_elements = build_elements(new_triples)

            existing = current_data.get("elements", [])
            existing_ids = {e["data"]["id"] for e in existing}
            for el in new_elements:
                if el["data"]["id"] not in existing_ids:
                    existing.append(el)

            return {"elements": existing, "expanded": expanded}

        return current_data

    @app.callback(
        [Output("graph", "elements"),
         Output("graph", "layout")],
        [Input("graph-data", "data"),
         Input("layout-select", "value")],
    )
    def render_graph(data, layout_name):
        elements = data.get("elements", []) if data else []
        layout = {"name": layout_name, "animate": True}
        if layout_name == "cola":
            layout["maxSimulationTime"] = 2000
        if layout_name == "dagre":
            layout["rankDir"] = "LR"
        return elements, layout

    @app.callback(
        Output("info-panel", "children"),
        [Input("graph", "tapNodeData"),
         Input("graph", "tapEdgeData")],
        prevent_initial_call=True,
    )
    def show_info(node_data, edge_data):
        ctx = callback_context
        if not ctx.triggered:
            return "Click a node or edge to see details."

        trigger = ctx.triggered[0]["prop_id"]

        if "tapNodeData" in trigger and node_data:
            node_id = node_data["id"]
            # Fetch all triples for this node to show properties
            outbound = query_by_bin(client, namespace, "subject", node_id)
            inbound = query_by_bin(client, namespace, "object", node_id)

            lines = [f"Node: {node_id}", f"Type: {node_type(node_id)}", ""]
            lines.append(f"Outbound edges: {len(outbound)}")
            for t in outbound:
                props = t.get("props", {})
                prop_str = f"  {json.dumps(props, default=str)}" if props else ""
                lines.append(f"  -[{t['predicate']}]-> {t['object']}{prop_str}")

            lines.append(f"\nInbound edges: {len(inbound)}")
            for t in inbound:
                props = t.get("props", {})
                prop_str = f"  {json.dumps(props, default=str)}" if props else ""
                lines.append(f"  {t['subject']} -[{t['predicate']}]->{prop_str}")

            return "\n".join(lines)

        if "tapEdgeData" in trigger and edge_data:
            lines = [
                f"Edge: {edge_data.get('label', '')}",
                f"Source: {edge_data.get('source', '')}",
                f"Target: {edge_data.get('target', '')}",
                "",
                "Properties:",
                edge_data.get("props", "{}"),
            ]
            return "\n".join(lines)

        return "Click a node or edge to see details."

    @app.callback(
        Output("stats-panel", "children"),
        Input("graph-data", "data"),
    )
    def update_stats(data):
        if not data or not data.get("elements"):
            return "No data loaded."
        elements = data["elements"]
        nodes = [e for e in elements if "source" not in e["data"]]
        edges = [e for e in elements if "source" in e["data"]]
        predicates = set(e["data"]["label"] for e in edges)
        node_types = {}
        for n in nodes:
            nt = n["data"].get("type", "default")
            node_types[nt] = node_types.get(nt, 0) + 1

        lines = [
            html.Div(f"Nodes: {len(nodes)}"),
            html.Div(f"Edges: {len(edges)}"),
            html.Div(f"Predicates: {', '.join(sorted(predicates))}"),
            html.Div(f"Expanded: {len(data.get('expanded', []))} nodes"),
            html.Hr(),
        ]
        for nt, count in sorted(node_types.items()):
            color = NODE_COLORS.get(nt, NODE_COLORS["default"])
            lines.append(html.Div([
                html.Span("\u25cf ", style={"color": color, "fontSize": "16px"}),
                html.Span(f"{nt}: {count}"),
            ]))
        return lines

    return app


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="RDF Property Graph Viewer")
    parser.add_argument("--host", default="127.0.0.1", help="Aerospike host (default: 127.0.0.1)")
    parser.add_argument("--port", type=int, default=3000, help="Aerospike port (default: 3000)")
    parser.add_argument("--namespace", default="test", help="Aerospike namespace (default: test)")
    parser.add_argument("--debug", action="store_true", help="Enable Dash debug mode")
    args = parser.parse_args()

    global client, namespace
    client, namespace = connect_aerospike(args.host, args.port, args.namespace)
    print(f"Connected to Aerospike at {args.host}:{args.port}, namespace={args.namespace}")

    app = create_app(client, namespace)
    print("Starting viewer at http://127.0.0.1:8050")
    app.run(debug=args.debug, host="0.0.0.0", port=8050)


if __name__ == "__main__":
    main()
