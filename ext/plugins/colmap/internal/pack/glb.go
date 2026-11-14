package pack

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// Mesh is a triangle mesh read from PLY.
type Mesh struct {
	Pos     []float32 // 3 per vertex
	Normals []float32 // 3 per vertex, or nil
	Colors  []uint8   // 3 per vertex (RGB), or nil
	Index   []uint32  // 3 per triangle
}

type plyProp struct {
	name     string
	typ      string // scalar type, or the item type of a list
	list     bool
	countTyp string
}

type plyElem struct {
	name  string
	count int
	props []plyProp
}

func plySize(t string) int {
	switch t {
	case "char", "int8", "uchar", "uint8":
		return 1
	case "short", "int16", "ushort", "uint16":
		return 2
	case "int", "int32", "uint", "uint32", "float", "float32":
		return 4
	case "double", "float64":
		return 8
	}
	return 0
}

func plyBin(b []byte, t string, order binary.ByteOrder) float64 {
	switch t {
	case "char", "int8":
		return float64(int8(b[0]))
	case "uchar", "uint8":
		return float64(b[0])
	case "short", "int16":
		return float64(int16(order.Uint16(b)))
	case "ushort", "uint16":
		return float64(order.Uint16(b))
	case "int", "int32":
		return float64(int32(order.Uint32(b)))
	case "uint", "uint32":
		return float64(order.Uint32(b))
	case "float", "float32":
		return float64(math.Float32frombits(order.Uint32(b)))
	}
	return math.Float64frombits(order.Uint64(b))
}

// ReadPLYMesh reads vertices (x y z, optional nx ny nz, red green blue) and
// faces (vertex_indices, fan-triangulated) from an ascii or binary PLY.
func ReadPLYMesh(p string) (*Mesh, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	if l, _ := r.ReadString('\n'); strings.TrimSpace(l) != "ply" {
		return nil, errors.New("not a PLY file")
	}
	var elems []*plyElem
	format := ""
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("PLY header: %w", err)
		}
		fs := strings.Fields(l)
		if len(fs) == 0 {
			continue
		}
		switch fs[0] {
		case "format":
			if len(fs) > 1 {
				format = fs[1]
			}
		case "element":
			if len(fs) < 3 {
				return nil, fmt.Errorf("PLY header: %q", l)
			}
			n, err := strconv.Atoi(fs[2])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("PLY header: %q", l)
			}
			elems = append(elems, &plyElem{name: fs[1], count: n})
		case "property":
			if len(elems) == 0 {
				return nil, errors.New("PLY property before element")
			}
			e := elems[len(elems)-1]
			if len(fs) == 5 && fs[1] == "list" {
				e.props = append(e.props, plyProp{name: fs[4], typ: fs[3], list: true, countTyp: fs[2]})
			} else if len(fs) == 3 {
				e.props = append(e.props, plyProp{name: fs[2], typ: fs[1]})
			} else {
				return nil, fmt.Errorf("PLY header: %q", l)
			}
		}
		if fs[0] == "end_header" {
			break
		}
	}
	var order binary.ByteOrder
	switch format {
	case "ascii":
	case "binary_little_endian":
		order = binary.LittleEndian
	case "binary_big_endian":
		order = binary.BigEndian
	default:
		return nil, fmt.Errorf("PLY format %q", format)
	}
	var words *bufio.Scanner
	if order == nil {
		words = bufio.NewScanner(r)
		words.Buffer(make([]byte, 1<<16), 1<<20)
		words.Split(bufio.ScanWords)
	}
	buf := make([]byte, 8)
	read := func(t string) (float64, error) {
		if order == nil {
			if !words.Scan() {
				return 0, io.ErrUnexpectedEOF
			}
			return strconv.ParseFloat(words.Text(), 64)
		}
		n := plySize(t)
		if n == 0 {
			return 0, fmt.Errorf("PLY type %q", t)
		}
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			return 0, err
		}
		return plyBin(buf[:n], t, order), nil
	}
	m := &Mesh{}
	for _, e := range elems {
		idx := map[string]int{}
		for i, pr := range e.props {
			idx[pr.name] = i
		}
		_, hasN := idx["nx"]
		_, hasC := idx["red"]
		if e.name == "vertex" {
			for _, k := range []string{"x", "y", "z"} {
				if _, ok := idx[k]; !ok {
					return nil, fmt.Errorf("PLY vertex has no %s", k)
				}
			}
			m.Pos = make([]float32, 0, 3*e.count)
			if hasN {
				m.Normals = make([]float32, 0, 3*e.count)
			}
			if hasC {
				m.Colors = make([]uint8, 0, 3*e.count)
			}
		}
		vals := make([]float64, len(e.props))
		var list []float64
		for i := 0; i < e.count; i++ {
			for j, pr := range e.props {
				if !pr.list {
					v, err := read(pr.typ)
					if err != nil {
						return nil, fmt.Errorf("PLY %s %d: %w", e.name, i, err)
					}
					vals[j] = v
					continue
				}
				n, err := read(pr.countTyp)
				if err != nil {
					return nil, fmt.Errorf("PLY %s %d: %w", e.name, i, err)
				}
				list = list[:0]
				for k := 0; k < int(n); k++ {
					v, err := read(pr.typ)
					if err != nil {
						return nil, fmt.Errorf("PLY %s %d: %w", e.name, i, err)
					}
					if pr.name == "vertex_indices" || pr.name == "vertex_index" {
						list = append(list, v)
					}
				}
			}
			switch e.name {
			case "vertex":
				m.Pos = append(m.Pos, float32(vals[idx["x"]]), float32(vals[idx["y"]]), float32(vals[idx["z"]]))
				if hasN {
					m.Normals = append(m.Normals, float32(vals[idx["nx"]]), float32(vals[idx["ny"]]), float32(vals[idx["nz"]]))
				}
				if hasC {
					m.Colors = append(m.Colors, uint8(vals[idx["red"]]), uint8(vals[idx["green"]]), uint8(vals[idx["blue"]]))
				}
			case "face":
				for k := 2; k < len(list); k++ {
					m.Index = append(m.Index, uint32(list[0]), uint32(list[k-1]), uint32(list[k]))
				}
			}
		}
	}
	nv := uint32(len(m.Pos) / 3)
	for _, v := range m.Index {
		if v >= nv {
			return nil, fmt.Errorf("PLY face index %d ≥ %d vertices", v, nv)
		}
	}
	return m, nil
}

// ZUpToYUp maps our frame (z up: ENU x east, y north, z up) to glTF's
// right-handed y-up: (x, y, z) → (x, z, −y). A rotation (−90° about x), so
// handedness and triangle winding are kept.
func ZUpToYUp(v []float32) {
	for i := 0; i+2 < len(v); i += 3 {
		v[i+1], v[i+2] = v[i+2], -v[i+1]
	}
}

// vertexNormals computes area-weighted vertex normals from the triangles.
func vertexNormals(m *Mesh) []float32 {
	n := make([]float32, len(m.Pos))
	p := func(i uint32) [3]float32 { return [3]float32{m.Pos[3*i], m.Pos[3*i+1], m.Pos[3*i+2]} }
	for t := 0; t+2 < len(m.Index); t += 3 {
		a, b, c := p(m.Index[t]), p(m.Index[t+1]), p(m.Index[t+2])
		u := [3]float32{b[0] - a[0], b[1] - a[1], b[2] - a[2]}
		v := [3]float32{c[0] - a[0], c[1] - a[1], c[2] - a[2]}
		x := [3]float32{u[1]*v[2] - u[2]*v[1], u[2]*v[0] - u[0]*v[2], u[0]*v[1] - u[1]*v[0]}
		for _, i := range m.Index[t : t+3] {
			n[3*i] += x[0]
			n[3*i+1] += x[1]
			n[3*i+2] += x[2]
		}
	}
	return n
}

func normalize(n []float32) {
	for i := 0; i+2 < len(n); i += 3 {
		l := float32(math.Sqrt(float64(n[i]*n[i] + n[i+1]*n[i+1] + n[i+2]*n[i+2])))
		if l == 0 || math.IsNaN(float64(l)) {
			n[i], n[i+1], n[i+2] = 0, 0, 1
			continue
		}
		n[i], n[i+1], n[i+2] = n[i]/l, n[i+1]/l, n[i+2]/l
	}
}

// WriteGLB writes m as binary glTF 2.0: POSITION, NORMAL (from the PLY, or
// computed), COLOR_0 (RGBA8 normalized, if the PLY has colours), uint32
// indices. Positions and normals are converted z-up → y-up first.
func WriteGLB(dst string, m *Mesh, generator string) (int64, error) {
	if len(m.Pos) == 0 || len(m.Index) == 0 {
		return 0, errors.New("mesh has no triangles")
	}
	nv := len(m.Pos) / 3
	normals := m.Normals
	if normals == nil {
		normals = vertexNormals(m)
	} else {
		normals = append([]float32(nil), normals...)
	}
	pos := append([]float32(nil), m.Pos...)
	ZUpToYUp(pos)
	ZUpToYUp(normals)
	normalize(normals)
	minP := []float32{math.MaxFloat32, math.MaxFloat32, math.MaxFloat32}
	maxP := []float32{-math.MaxFloat32, -math.MaxFloat32, -math.MaxFloat32}
	for i := 0; i < len(pos); i += 3 {
		for k := 0; k < 3; k++ {
			minP[k] = min(minP[k], pos[i+k])
			maxP[k] = max(maxP[k], pos[i+k])
		}
	}
	var bin bytes.Buffer
	type view struct {
		Buffer     int `json:"buffer"`
		ByteOffset int `json:"byteOffset"`
		ByteLength int `json:"byteLength"`
		Target     int `json:"target"`
	}
	var views []view
	add := func(data any, target int) int {
		off := bin.Len()
		_ = binary.Write(&bin, binary.LittleEndian, data)
		for bin.Len()%4 != 0 {
			bin.WriteByte(0)
		}
		views = append(views, view{0, off, bin.Len() - off, target})
		return len(views) - 1
	}
	const arrayBuf, elemBuf = 34962, 34963
	accessors := []map[string]any{
		{"bufferView": add(pos, arrayBuf), "componentType": 5126, "count": nv, "type": "VEC3", "min": minP, "max": maxP},
		{"bufferView": add(normals, arrayBuf), "componentType": 5126, "count": nv, "type": "VEC3"},
	}
	attrs := map[string]int{"POSITION": 0, "NORMAL": 1}
	if m.Colors != nil {
		rgba := make([]uint8, 0, 4*nv)
		for i := 0; i < nv; i++ {
			rgba = append(rgba, m.Colors[3*i], m.Colors[3*i+1], m.Colors[3*i+2], 255)
		}
		accessors = append(accessors, map[string]any{"bufferView": add(rgba, arrayBuf), "componentType": 5121, "normalized": true, "count": nv, "type": "VEC4"})
		attrs["COLOR_0"] = len(accessors) - 1
	}
	accessors = append(accessors, map[string]any{"bufferView": add(m.Index, elemBuf), "componentType": 5125, "count": len(m.Index), "type": "SCALAR"})
	doc := map[string]any{
		"asset":       map[string]any{"version": "2.0", "generator": generator},
		"scene":       0,
		"scenes":      []any{map[string]any{"nodes": []int{0}}},
		"nodes":       []any{map[string]any{"mesh": 0}},
		"meshes":      []any{map[string]any{"primitives": []any{map[string]any{"attributes": attrs, "indices": len(accessors) - 1, "mode": 4}}}},
		"buffers":     []any{map[string]any{"byteLength": bin.Len()}},
		"bufferViews": views,
		"accessors":   accessors,
	}
	js, err := json.Marshal(doc)
	if err != nil {
		return 0, err
	}
	for len(js)%4 != 0 {
		js = append(js, ' ')
	}
	total := 12 + 8 + len(js) + 8 + bin.Len()
	f, err := os.Create(dst + ".part")
	if err != nil {
		return 0, err
	}
	defer os.Remove(dst + ".part")
	w := bufio.NewWriter(f)
	le := binary.LittleEndian
	_ = binary.Write(w, le, []uint32{0x46546C67, 2, uint32(total), uint32(len(js)), 0x4E4F534A})
	w.Write(js)
	_ = binary.Write(w, le, []uint32{uint32(bin.Len()), 0x004E4942})
	w.Write(bin.Bytes())
	if err := w.Flush(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	return int64(total), os.Rename(dst+".part", dst)
}
