package tarfmt

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"strings"

	importer "github.com/ipfs/go-ipfs/importer"
	chunk "github.com/ipfs/go-ipfs/importer/chunk"
	dag "github.com/ipfs/go-ipfs/merkledag"
	dagutil "github.com/ipfs/go-ipfs/merkledag/utils"
	path "github.com/ipfs/go-ipfs/path"
	uio "github.com/ipfs/go-ipfs/unixfs/io"
	logging "gx/ipfs/QmaDNZ4QMdBdku1YZWBysufYyoQt1negQGNav6PLYarbY8/go-log"

	context "gx/ipfs/QmZy2y8t9zQH2a1b8q2ZSLKp17ATuJoCNxxyMFG5qFExpt/go-net/context"
)

var log = logging.Logger("tarfmt")

var blockSize = 512
var zeroBlock = make([]byte, blockSize)

func marshalHeader(h *tar.Header) ([]byte, error) {
	buf := new(bytes.Buffer)
	w := tar.NewWriter(buf)
	err := w.WriteHeader(h)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ImportTar(r io.Reader, ds dag.DAGService) (*dag.Node, error) {
	rall, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	r = bytes.NewReader(rall)

	tr := tar.NewReader(r)

	root := new(dag.Node)
	root.Data = []byte("ipfs/tar")

	e := dagutil.NewDagEditor(root, ds)

	for {
		h, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		header := new(dag.Node)

		headerBytes, err := marshalHeader(h)
		if err != nil {
			return nil, err
		}

		header.Data = headerBytes

		if h.Size > 0 {
			spl := chunk.NewRabin(tr, uint64(chunk.DefaultBlockSize))
			nd, err := importer.BuildDagFromReader(ds, spl)
			if err != nil {
				return nil, err
			}

			err = header.AddNodeLinkClean("data", nd)
			if err != nil {
				return nil, err
			}
		}

		_, err = ds.Add(header)
		if err != nil {
			return nil, err
		}

		path := escapePath(h.Name)
		err = e.InsertNodeAtPath(context.Background(), path, header, func() *dag.Node { return new(dag.Node) })
		if err != nil {
			return nil, err
		}
	}

	return e.Finalize(ds)
}

// adds a '-' to the beginning of each path element so we can use 'data' as a
// special link in the structure without having to worry about
func escapePath(pth string) string {
	elems := path.SplitList(strings.Trim(pth, "/"))
	for i, e := range elems {
		elems[i] = "-" + e
	}
	return path.Join(elems)
}

type tarReader struct {
	links []*dag.Link
	ds    dag.DAGService

	childRead *tarReader
	hdrBuf    *bytes.Reader
	fileRead  *countReader
	pad       int

	ctx context.Context
}

func (tr *tarReader) Read(b []byte) (int, error) {
	// if we have a header to be read, it takes priority
	if tr.hdrBuf != nil {
		n, err := tr.hdrBuf.Read(b)
		if err == io.EOF {
			tr.hdrBuf = nil
			return n, nil
		}
		return n, err
	}

	// no header remaining, check for recursive
	if tr.childRead != nil {
		n, err := tr.childRead.Read(b)
		if err == io.EOF {
			tr.childRead = nil
			return n, nil
		}
		return n, err
	}

	// check for filedata to be read
	if tr.fileRead != nil {
		n, err := tr.fileRead.Read(b)
		if err == io.EOF {
			nr := tr.fileRead.n
			tr.pad = (blockSize - (nr % blockSize)) % blockSize
			tr.fileRead.Close()
			tr.fileRead = nil
			return n, nil
		}
		return n, err
	}

	// filedata reads must be padded out to 512 byte offsets
	if tr.pad > 0 {
		n := copy(b, zeroBlock[:tr.pad])
		tr.pad -= n
		return n, nil
	}

	if len(tr.links) == 0 {
		return 0, io.EOF
	}

	next := tr.links[0]
	tr.links = tr.links[1:]

	headerNd, err := next.GetNode(tr.ctx, tr.ds)
	if err != nil {
		return 0, err
	}

	tr.hdrBuf = bytes.NewReader(headerNd.Data)

	dataNd, err := headerNd.GetLinkedNode(tr.ctx, tr.ds, "data")
	if err != nil && err != dag.ErrLinkNotFound {
		return 0, err
	}

	if err == nil {
		dr, err := uio.NewDagReader(tr.ctx, dataNd, tr.ds)
		if err != nil {
			log.Error("dagreader error: ", err)
			return 0, err
		}

		tr.fileRead = &countReader{r: dr}
	} else if len(headerNd.Links) > 0 {
		tr.childRead = &tarReader{
			links: headerNd.Links,
			ds:    tr.ds,
			ctx:   tr.ctx,
		}
	}

	return tr.Read(b)
}

func ExportTar(ctx context.Context, root *dag.Node, ds dag.DAGService) (io.Reader, error) {
	if string(root.Data) != "ipfs/tar" {
		return nil, errors.New("not an ipfs tarchive")
	}
	return &tarReader{
		links: root.Links,
		ds:    ds,
		ctx:   ctx,
	}, nil
}

type countReader struct {
	r io.ReadCloser
	n int
}

func (r *countReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.n += n
	return n, err
}

func (r *countReader) Close() error {
	return r.r.Close()
}
