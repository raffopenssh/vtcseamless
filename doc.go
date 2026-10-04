// Package vtcseamless reassembles seamless polygons from a vector-tile cache.
//
// Tile servers clip every feature to the tile it is drawn in, usually WITH A
// BUFFER of a few pixels so that strokes do not show seams at tile edges. For
// rendering that is right; for data it is wrong: the per-tile copies of one
// polygon overlap, their areas double-count along every tile edge, and a plain
// union of the copies has the wrong area wherever the buffer overlapped a
// neighbour. The engine in this package applies one rule that makes the
// problem go away:
//
//  1. ClipToBound: every piece is clipped back to its own tile rectangle
//     (Sutherland–Hodgman). Tiles tessellate, so the clipped pieces of one
//     feature are DISJOINT, their areas add, and the union cannot double-count.
//  2. UnionPieces: pieces of one feature id are dissolved with polyclip. The
//     result is accepted only if its area agrees with the additive area of the
//     pieces within UnionAreaTol (2 %); otherwise the pieces are kept as they
//     are (a seam remains along the tile edge, but nothing is invented).
//  3. ClassifyRings: polyclip returns a flat, unordered contour list; rings are
//     turned into polygons by nesting depth (even = shell, odd = hole) and
//     oriented shell CCW / holes CW (RFC 7946).
//  4. MergeSeams: for layers WITHOUT a stable feature id, pieces from adjacent
//     tiles that share a stretch of the common edge are unioned when that
//     union really dissolves the seam (fewer polygons out than in).
//
// Everything here is planar lon/lat geometry on orb types; it knows nothing
// about any particular tile server. Package bevdirect is the preset for the
// Austrian cadastre tile cache and shows how the pieces fit.
package vtcseamless
