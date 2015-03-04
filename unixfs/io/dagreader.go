package io

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"os"

	proto "github.com/jbenet/go-ipfs/Godeps/_workspace/src/code.google.com/p/goprotobuf/proto"
	"github.com/jbenet/go-ipfs/Godeps/_workspace/src/golang.org/x/net/context"
	mdag "github.com/jbenet/go-ipfs/merkledag"
	ft "github.com/jbenet/go-ipfs/unixfs"
	ftpb "github.com/jbenet/go-ipfs/unixfs/pb"
)

var ErrIsDir = errors.New("this dag node is a directory")

// DagReader provides a way to easily read the data contained in a dag.
type DagReader struct {
	serv mdag.DAGService

	// the node being read
	node *mdag.Node

	// cached protobuf structure from node.Data
	pbdata *ftpb.Data

	// the current data buffer to be read from
	// will either be a bytes.Reader or a child DagReader
	buf ReadSeekCloser

	// NodeGetters for each of 'nodes' child links
	promises []mdag.NodeGetter

	// the index of the child link currently being read from
	linkPosition int

	// current offset for the read head within the 'file'
	offset int64

	// Our context
	ctx context.Context

	// context cancel for children
	cancel func()
}

type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
	io.WriterTo
}

// NewDagReader creates a new reader object that reads the data represented by the given
// node, using the passed in DAGService for data retreival
func NewDagReader(ctx context.Context, n *mdag.Node, serv mdag.DAGService) (*DagReader, error) {
	pb := new(ftpb.Data)
	err := proto.Unmarshal(n.Data, pb)
	if err != nil {
		return nil, err
	}

	switch pb.GetType() {
	case ftpb.Data_Directory:
		// Dont allow reading directories
		return nil, ErrIsDir
	case ftpb.Data_Raw:
		fallthrough
	case ftpb.Data_File:
		return newDataFileReader(ctx, n, pb, serv), nil
	case ftpb.Data_Metadata:
		if len(n.Links) == 0 {
			return nil, errors.New("incorrectly formatted metadata object")
		}
		child, err := n.Links[0].GetNode(serv)
		if err != nil {
			return nil, err
		}
		return NewDagReader(ctx, child, serv)
	default:
		return nil, ft.ErrUnrecognizedType
	}
}

func newDataFileReader(ctx context.Context, n *mdag.Node, pb *ftpb.Data, serv mdag.DAGService) *DagReader {
	fctx, cancel := context.WithCancel(ctx)
	promises := serv.GetDAG(fctx, n)
	return &DagReader{
		node:     n,
		serv:     serv,
		buf:      NewRSNCFromBytes(pb.GetData()),
		promises: promises,
		ctx:      fctx,
		cancel:   cancel,
		pbdata:   pb,
	}
}

// precalcNextBuf follows the next link in line and loads it from the DAGService,
// setting the next buffer to read from
func (dr *DagReader) precalcNextBuf() error {
	dr.buf.Close() // Just to make sure
	if dr.linkPosition >= len(dr.promises) {
		return io.EOF
	}
	nxt, err := dr.promises[dr.linkPosition].Get()
	if err != nil {
		return err
	}
	dr.linkPosition++

	pb := new(ftpb.Data)
	err = proto.Unmarshal(nxt.Data, pb)
	if err != nil {
		return err
	}

	switch pb.GetType() {
	case ftpb.Data_Directory:
		// A directory should not exist within a file
		return ft.ErrInvalidDirLocation
	case ftpb.Data_File:
		dr.buf = newDataFileReader(dr.ctx, nxt, pb, dr.serv)
		return nil
	case ftpb.Data_Raw:
		dr.buf = NewRSNCFromBytes(pb.GetData())
		return nil
	case ftpb.Data_Metadata:
		return errors.New("Shouldnt have had metadata object inside file")
	default:
		return ft.ErrUnrecognizedType
	}
}

// Size return the total length of the data from the DAG structured file.
func (dr *DagReader) Size() int64 {
	return int64(dr.pbdata.GetFilesize())
}

// Read reads data from the DAG structured file
func (dr *DagReader) Read(b []byte) (int, error) {
	// If no cached buffer, load one
	total := 0
	for {
		// Attempt to fill bytes from cached buffer
		n, err := dr.buf.Read(b[total:])
		total += n
		dr.offset += int64(n)
		if err != nil {
			// EOF is expected
			if err != io.EOF {
				return total, err
			}
		}

		// If weve read enough bytes, return
		if total == len(b) {
			return total, nil
		}

		// Otherwise, load up the next block
		err = dr.precalcNextBuf()
		if err != nil {
			return total, err
		}
	}
}

func (dr *DagReader) WriteTo(w io.Writer) (int64, error) {
	// If no cached buffer, load one
	total := int64(0)
	for {
		// Attempt to write bytes from cached buffer
		n, err := dr.buf.WriteTo(w)
		total += n
		dr.offset += n
		if err != nil {
			if err != io.EOF {
				return total, err
			}
		}

		// Otherwise, load up the next block
		err = dr.precalcNextBuf()
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}

func (dr *DagReader) Close() error {
	dr.cancel()
	return nil
}

// Seek implements io.Seeker, and will seek to a given offset in the file
// interface matches standard unix seek
// TODO: check if we can do relative seeks, to reduce the amount of dagreader
// recreations that need to happen.
func (dr *DagReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case os.SEEK_SET:
		if offset < 0 {
			return -1, errors.New("Invalid offset")
		}

		// Grab cached protobuf object (solely to make code look cleaner)
		pb := dr.pbdata

		// left represents the number of bytes remaining to seek to (from beginning)
		left := offset
		if int64(len(pb.Data)) >= offset {
			// Close current buf to close potential child dagreader
			dr.buf.Close()
			dr.buf = NewRSNCFromBytes(pb.GetData()[offset:])

			// start reading links from the beginning
			dr.linkPosition = 0
			dr.offset = offset
			return offset, nil
		} else {
			// skip past root block data
			left -= int64(len(pb.Data))
		}

		// iterate through links and find where we need to be
		for i := 0; i < len(pb.Blocksizes); i++ {
			if pb.Blocksizes[i] > uint64(left) {
				dr.linkPosition = i
				break
			} else {
				left -= int64(pb.Blocksizes[i])
			}
		}

		// start sub-block request
		err := dr.precalcNextBuf()
		if err != nil {
			return 0, err
		}

		// set proper offset within child readseeker
		n, err := dr.buf.Seek(left, os.SEEK_SET)
		if err != nil {
			return -1, err
		}

		// sanity
		left -= n
		if left != 0 {
			return -1, errors.New("failed to seek properly")
		}
		dr.offset = offset
		return offset, nil
	case os.SEEK_CUR:
		// TODO: be smarter here
		noffset := dr.offset + offset
		return dr.Seek(noffset, os.SEEK_SET)
	case os.SEEK_END:
		noffset := int64(dr.pbdata.GetFilesize()) - offset
		return dr.Seek(noffset, os.SEEK_SET)
	default:
		return 0, errors.New("invalid whence")
	}
	return 0, nil
}

// readSeekNopCloser wraps a bytes.Reader to implement ReadSeekCloser
type readSeekNopCloser struct {
	*bytes.Reader
}

func NewRSNCFromBytes(b []byte) ReadSeekCloser {
	return &readSeekNopCloser{bytes.NewReader(b)}
}

func (r *readSeekNopCloser) Close() error { return nil }

func ReadAll(ctx context.Context, nd *mdag.Node, dserv mdag.DAGService) ([]byte, error) {
	read, err := NewDagReader(ctx, nd, dserv)
	if err != nil {
		return nil, err
	}

	return ioutil.ReadAll(read)
}
