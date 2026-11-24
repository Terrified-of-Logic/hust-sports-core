package sports

import (
	"fmt"
	"math"
)

// Alignment reports how far observed points lie from the reference polyline.
// Each observation contributes equally to the mean.
type Alignment struct {
	PointCount       int
	WithinTolerance  int
	MeanOffsetMeters float64
	MaxOffsetMeters  float64
}

// AssessAlignment measures the distance from each observed point to its nearest
// reference edge. Both tracks must use the same CRS. Tolerance is nonnegative
// and includes points on the boundary. Close a loop by repeating its first
// point at the end of the segment. Reference segment gaps are never bridged.
func AssessAlignment(observed, reference Track, toleranceMeters float64) (Alignment, error) {
	if toleranceMeters < 0 || math.IsNaN(toleranceMeters) || math.IsInf(toleranceMeters, 0) {
		return Alignment{}, fmt.Errorf("%w: tolerance must be finite and nonnegative", ErrInvalidTrack)
	}
	if err := observed.Validate(); err != nil {
		return Alignment{}, fmt.Errorf("observed track: %w", err)
	}
	if err := reference.Validate(); err != nil {
		return Alignment{}, fmt.Errorf("reference track: %w", err)
	}
	if observed.CRS != reference.CRS {
		return Alignment{}, fmt.Errorf("%w: observed %s, reference %s", ErrCRS, observed.CRS, reference.CRS)
	}
	edges, err := referenceEdges(reference)
	if err != nil {
		return Alignment{}, err
	}

	var result Alignment
	var sum float64
	for _, segment := range observed.Segments {
		for _, point := range segment {
			position := unitPosition(point)
			nearest := math.Inf(1)
			for _, edge := range edges {
				nearest = min(nearest, edge.distanceTo(position))
			}
			result.PointCount++
			if nearest <= toleranceMeters {
				result.WithinTolerance++
			}
			sum += nearest
			result.MaxOffsetMeters = max(result.MaxOffsetMeters, nearest)
		}
	}
	result.MeanOffsetMeters = sum / float64(result.PointCount)
	return result, nil
}

type edge struct {
	start, end               vector
	normal                   vector
	startTangent, endTangent vector
}

func referenceEdges(reference Track) ([]edge, error) {
	const minCrossNorm = 1e-14
	var edges []edge
	for i, segment := range reference.Segments {
		start := unitPosition(segment[0])
		for j, point := range segment[1:] {
			end := unitPosition(point)
			cross := start.cross(end)
			norm := cross.norm()
			if norm > minCrossNorm {
				normal := cross.scale(1 / norm)
				edges = append(edges, edge{start, end, normal, normal.cross(start), normal.cross(end)})
			} else if start.dot(end) < 0 {
				return nil, fmt.Errorf("%w: reference segment %d edge %d is antipodal", ErrInvalidTrack, i, j)
			}
			start = end
		}
	}
	if len(edges) == 0 {
		return nil, fmt.Errorf("%w: reference has no nonzero edges", ErrInvalidTrack)
	}
	return edges, nil
}

func (e edge) distanceTo(p vector) float64 {
	if p == e.start || p == e.end {
		return 0
	}
	// The tangent tests determine whether the perpendicular foot lies on the
	// minor arc. Otherwise an endpoint is the nearest point.
	if p.dot(e.startTangent) >= 0 && p.dot(e.endTangent) <= 0 {
		return earthRadiusMeters * math.Asin(min(1, math.Abs(p.dot(e.normal))))
	}
	return min(distanceMeters(p, e.start), distanceMeters(p, e.end))
}
