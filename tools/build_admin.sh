#!/usr/bin/env bash
# Rebuild bevdirect/admin.json.gz from BEV OGD "Verwaltungsgrenzen (VGD) 1:50 000"
# (CC BY 4.0, https://data.bev.gv.at). No other source is involved:
# names/codes/bboxes/areas come from the official KG polygons only.
#
#   tools/build_admin.sh [STICHTAG_DIR [FILE_DATE]]     e.g. 20260401 20260402
#
# Needs: curl, unzip, ogr2ogr (gdal-bin), python3.
set -euo pipefail
DIR=${1:-20260401}; FDATE=${2:-20260402}
URL="https://data.bev.gv.at/download/Verwaltungsgrenzen/shp/${DIR}/VGD_Oesterreich_gen_50_${FDATE}.zip"
HERE=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
echo "fetching $URL"
curl -sfL "$URL" -o "$WORK/vgd.zip"
unzip -q -o "$WORK/vgd.zip" -d "$WORK"
SHP=$(ls "$WORK"/*.shp | head -1)
# MGI/Austria Lambert (EPSG:31287) → WGS84, GeoJSON, keep only the admin attributes.
ogr2ogr -f GeoJSON -t_srs EPSG:4326 -lco COORDINATE_PRECISION=5 \
  -select KG_NR,KG,GKZ,PG,PB,BL "$WORK/vgd.geojson" "$SHP"
python3 - "$WORK/vgd.geojson" "$HERE/bevdirect/admin.json.gz" "$DIR" <<'PY'
import gzip, json, math, sys
src, dst, stichtag = sys.argv[1:4]
fc = json.load(open(src))
R = 6371008.8
def ring_area(ring):  # spherical excess, m²
    a = 0.0
    for i in range(len(ring) - 1):
        l1, p1 = map(math.radians, ring[i]); l2, p2 = map(math.radians, ring[i + 1])
        a += (l2 - l1) * (2 + math.sin(p1) + math.sin(p2))
    return a * R * R / 2
rows = []
for f in fc["features"]:
    p = f["properties"]
    polys = []
    def collect(g):  # a few KGs come out as GeometryCollection (polygon + stray lines)
        if g["type"] == "Polygon": polys.append(g["coordinates"])
        elif g["type"] == "MultiPolygon": polys.extend(g["coordinates"])
        elif g["type"] == "GeometryCollection":
            for sub in g["geometries"]: collect(sub)
    collect(f["geometry"])
    if not polys:
        print("no polygon for", p["KG_NR"], file=sys.stderr); continue
    xs = [c[0] for poly in polys for ring in poly for c in ring]
    ys = [c[1] for poly in polys for ring in poly for c in ring]
    area = sum(abs(ring_area(poly[0])) - sum(abs(ring_area(h)) for h in poly[1:]) for poly in polys)
    rows.append({
        "kg_code": p["KG_NR"].zfill(5), "kg_name": p["KG"],
        "gemeinde_code": p["GKZ"], "gemeinde_name": p["PG"],
        "district_name": p["PB"], "state_name": p["BL"],
        "min_lon": round(min(xs), 4), "min_lat": round(min(ys), 4),
        "max_lon": round(max(xs), 4), "max_lat": round(max(ys), 4),
        "area_sqkm": round(area / 1e6, 3),
    })
rows.sort(key=lambda r: r["kg_code"])
doc = {"source": "BEV Verwaltungsgrenzen (VGD) 1:50 000, Stichtag %s-%s-%s, CC BY 4.0" % (stichtag[:4], stichtag[4:6], stichtag[6:]),
       "source_url": "https://data.bev.gv.at/geonetwork/srv/ger/catalog.search#/search?any=Verwaltungsgrenzen%20VGD",
       "kgs": rows}
with gzip.open(dst, "wt", encoding="utf-8", compresslevel=9) as f:
    json.dump(doc, f, ensure_ascii=False, separators=(",", ":"))
print("wrote", dst, len(rows), "KGs")
PY
