package priors

import "math"

// WGS84 ellipsoid.
const (
	wgsA  = 6378137.0
	wgsF  = 1 / 298.257223563
	wgsE2 = wgsF * (2 - wgsF)
)

// ToECEF converts a WGS84 position (degrees, metres above the ellipsoid) to
// earth-centred earth-fixed metres.
func ToECEF(p LLA) [3]float64 {
	lat, lon := p.Lat*math.Pi/180, p.Lon*math.Pi/180
	sl, cl := math.Sin(lat), math.Cos(lat)
	n := wgsA / math.Sqrt(1-wgsE2*sl*sl)
	return [3]float64{
		(n + p.Alt) * cl * math.Cos(lon),
		(n + p.Alt) * cl * math.Sin(lon),
		(n*(1-wgsE2) + p.Alt) * sl,
	}
}

// ToENU is p in east-north-up metres about origin.
func ToENU(p, origin LLA) [3]float64 {
	a, o := ToECEF(p), ToECEF(origin)
	d := [3]float64{a[0] - o[0], a[1] - o[1], a[2] - o[2]}
	lat, lon := origin.Lat*math.Pi/180, origin.Lon*math.Pi/180
	sl, cl, so, co := math.Sin(lat), math.Cos(lat), math.Sin(lon), math.Cos(lon)
	return [3]float64{
		-so*d[0] + co*d[1],
		-sl*co*d[0] - sl*so*d[1] + cl*d[2],
		cl*co*d[0] + cl*so*d[1] + sl*d[2],
	}
}
