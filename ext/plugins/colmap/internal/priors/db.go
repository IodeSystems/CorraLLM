package priors

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"

	_ "modernc.org/sqlite"
)

// COLMAP 4.2.1 enums (geometry/pose_prior.h, util/types.h).
const (
	CoordUndefined   = -1
	CoordWGS84       = 0
	CoordCartesian   = 1
	SensorTypeCamera = 0
)

// DBImage is one row of COLMAP's images table.
type DBImage struct {
	ID       int64
	Name     string
	CameraID int64
}

// EncodeVec is an Eigen Vector3d blob: 3 little-endian float64.
func EncodeVec(v [3]float64) []byte {
	b := make([]byte, 24)
	for i, x := range v {
		binary.LittleEndian.PutUint64(b[i*8:], math.Float64bits(x))
	}
	return b
}

// EncodeMat is an Eigen Matrix3d blob: 9 little-endian float64, column-major.
// m is row-major.
func EncodeMat(m [9]float64) []byte {
	b := make([]byte, 72)
	k := 0
	for col := 0; col < 3; col++ {
		for row := 0; row < 3; row++ {
			binary.LittleEndian.PutUint64(b[k*8:], math.Float64bits(m[row*3+col]))
			k++
		}
	}
	return b
}

// DecodeVec reads EncodeVec's layout.
func DecodeVec(b []byte) ([3]float64, bool) {
	var v [3]float64
	if len(b) != 24 {
		return v, false
	}
	for i := range v {
		v[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[i*8:]))
	}
	return v, true
}

// DecodeMat reads EncodeMat's layout back to row-major.
func DecodeMat(b []byte) ([9]float64, bool) {
	var m [9]float64
	if len(b) != 72 {
		return m, false
	}
	k := 0
	for col := 0; col < 3; col++ {
		for row := 0; row < 3; row++ {
			m[row*3+col] = math.Float64frombits(binary.LittleEndian.Uint64(b[k*8:]))
			k++
		}
	}
	return m, true
}

func openColmapDB(path string) (*sql.DB, error) {
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	return d, nil
}

// ReadDB reads the images and the WGS84 position rows feature_extractor wrote.
func ReadDB(path string) ([]DBImage, map[string]LLA, error) {
	d, err := openColmapDB(path)
	if err != nil {
		return nil, nil, err
	}
	defer d.Close()
	rows, err := d.Query(`SELECT image_id, name, camera_id FROM images ORDER BY image_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("read images: %w", err)
	}
	var imgs []DBImage
	byID := map[int64]string{}
	for rows.Next() {
		var im DBImage
		if err := rows.Scan(&im.ID, &im.Name, &im.CameraID); err != nil {
			rows.Close()
			return nil, nil, err
		}
		imgs = append(imgs, im)
		byID[im.ID] = im.Name
	}
	rows.Close()
	exif := map[string]LLA{}
	prow, err := d.Query(`SELECT corr_data_id, position, coordinate_system FROM pose_priors WHERE corr_sensor_type=?`, SensorTypeCamera)
	if err != nil {
		return nil, nil, fmt.Errorf("read pose_priors: %w", err)
	}
	defer prow.Close()
	for prow.Next() {
		var id int64
		var pos []byte
		var cs int
		if err := prow.Scan(&id, &pos, &cs); err != nil {
			return nil, nil, err
		}
		v, ok := DecodeVec(pos)
		if cs != CoordWGS84 || !ok || math.IsNaN(v[0]) || byID[id] == "" {
			continue
		}
		exif[byID[id]] = LLA{v[0], v[1], v[2]}
	}
	return imgs, exif, prow.Err()
}

// WriteDB replaces every pose_priors row with the plan's rows, CARTESIAN.
// Gravity-only rows go too (§1c-4); images without a prior get no row.
func WriteDB(path string, imgs []DBImage, plan Plan) (int, error) {
	d, err := openColmapDB(path)
	if err != nil {
		return 0, err
	}
	defer d.Close()
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM pose_priors`); err != nil {
		return 0, err
	}
	nan := EncodeVec([3]float64{math.NaN(), math.NaN(), math.NaN()})
	n := 0
	for _, im := range imgs {
		r, ok := plan.Rows[im.Name]
		if !ok {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO pose_priors(corr_data_id, corr_sensor_id, corr_sensor_type, position, position_covariance, gravity, coordinate_system) VALUES(?,?,?,?,?,?,?)`,
			im.ID, im.CameraID, SensorTypeCamera, EncodeVec(r.Pos), EncodeMat(r.Cov), nan, CoordCartesian); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}
