package commands

import (
	"fmt"
	"io"
	"text/tabwriter"

	cmdenv "github.com/ipfs/go-ipfs/core/commands/cmdenv"
	e "github.com/ipfs/go-ipfs/core/commands/e"
	iface "github.com/ipfs/go-ipfs/core/coreapi/interface"

	cid "gx/ipfs/QmPSQnBKM9g7BaUcZCvswUJVscQ1ipjmwxN5PXCjkp9EQ7/go-cid"
	ipld "gx/ipfs/QmR7TcHkR9nxkUorfi8XMTAMLUK7GiP64TWWBzY3aacc1o/go-ipld-format"
	cmds "gx/ipfs/QmSXUokcP4TJpFfqozT69AVAYRtzXVMUjzQVkYX41R9Svs/go-ipfs-cmds"
	merkledag "gx/ipfs/QmSei8kFMfqdJq7Q68d2LMnHbTWKKg2daA29ezUYFAUNgc/go-merkledag"
	offline "gx/ipfs/QmT6dHGp3UYd3vUMpy7rzX2CXQv7HLcj42Vtq8qwwjgASb/go-ipfs-exchange-offline"
	unixfs "gx/ipfs/QmUaZkqxmKvUX16F8XeAAk9LVvmNMktvbhcx4PG4s8SqDG/go-unixfs"
	uio "gx/ipfs/QmUaZkqxmKvUX16F8XeAAk9LVvmNMktvbhcx4PG4s8SqDG/go-unixfs/io"
	unixfspb "gx/ipfs/QmUaZkqxmKvUX16F8XeAAk9LVvmNMktvbhcx4PG4s8SqDG/go-unixfs/pb"
	blockservice "gx/ipfs/QmWfhv1D18DRSiSm73r4QGcByspzPtxxRTcmHW3axFXZo8/go-blockservice"
	"gx/ipfs/Qmde5VP1qUkyQXKCfmEUA7bP64V2HAptbJ7phuPp7jXWwg/go-ipfs-cmdkit"
)

// LsLink contains printable data for a single ipld link in ls output
type LsLink struct {
	Name, Hash string
	Size       uint64
	Type       unixfspb.Data_DataType
}

// LsObject is an element of LsOutput
// It can represent a whole directory, a directory header, one or more links,
// Or a the end of a directory
type LsObject struct {
	Hash      string
	Links     []LsLink
	HasHeader bool
	HasLinks  bool
	HasFooter bool
}

// LsOutput is a set of printable data for directories
type LsOutput struct {
	MultipleFolders bool
	Objects         []LsObject
}

const (
	lsHeadersOptionNameTime = "headers"
	lsResolveTypeOptionName = "resolve-type"
	lsStreamOptionName      = "stream"
)

var LsCmd = &cmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "List directory contents for Unix filesystem objects.",
		ShortDescription: `
Displays the contents of an IPFS or IPNS object(s) at the given path, with
the following format:

  <link base58 hash> <link size in bytes> <link name>

The JSON output contains type information.
`,
	},

	Arguments: []cmdkit.Argument{
		cmdkit.StringArg("ipfs-path", true, true, "The path to the IPFS object(s) to list links from.").EnableStdin(),
	},
	Options: []cmdkit.Option{
		cmdkit.BoolOption(lsHeadersOptionNameTime, "v", "Print table headers (Hash, Size, Name)."),
		cmdkit.BoolOption(lsResolveTypeOptionName, "Resolve linked objects to find out their types.").WithDefault(true),
		cmdkit.BoolOption(lsStreamOptionName, "s", "Stream directory entries as they are found."),
	},
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		nd, err := cmdenv.GetNode(env)
		if err != nil {
			return err
		}

		api, err := cmdenv.GetApi(env)
		if err != nil {
			return err
		}

		resolve, _ := req.Options[lsResolveTypeOptionName].(bool)
		dserv := nd.DAG
		if !resolve {
			offlineexch := offline.Exchange(nd.Blockstore)
			bserv := blockservice.New(nd.Blockstore, offlineexch)
			dserv = merkledag.NewDAGService(bserv)
		}

		err = req.ParseBodyArgs()
		if err != nil {
			return err
		}

		paths := req.Arguments

		var dagnodes []ipld.Node
		for _, fpath := range paths {
			p, err := iface.ParsePath(fpath)
			if err != nil {
				return err
			}

			dagnode, err := api.ResolveNode(req.Context, p)
			if err != nil {
				return err
			}
			dagnodes = append(dagnodes, dagnode)
		}
		ng := merkledag.NewSession(req.Context, nd.DAG)
		ro := merkledag.NewReadOnlyDagService(ng)

		stream, _ := req.Options[lsStreamOptionName].(bool)
		multipleFolders := len(req.Arguments) > 1
		if !stream {
			output := make([]LsObject, len(req.Arguments))

			for i, dagnode := range dagnodes {
				dir, err := uio.NewDirectoryFromNode(ro, dagnode)
				if err != nil && err != uio.ErrNotADir {
					return fmt.Errorf("the data in %s (at %q) is not a UnixFS directory: %s", dagnode.Cid(), paths[i], err)
				}

				var links []*ipld.Link
				if dir == nil {
					links = dagnode.Links()
				} else {
					links, err = dir.Links(req.Context)
					if err != nil {
						return err
					}
				}
				outputLinks := make([]LsLink, len(links))
				for j, link := range links {
					lsLink, err := makeLsLink(req, dserv, resolve, link)
					if err != nil {
						return err
					}
					outputLinks[j] = *lsLink
				}
				output[i] = newFullDirectoryLsObject(paths[i], outputLinks)
			}

			return cmds.EmitOnce(res, &LsOutput{multipleFolders, output})
		}

		for i, dagnode := range dagnodes {
			dir, err := uio.NewDirectoryFromNode(ro, dagnode)
			if err != nil && err != uio.ErrNotADir {
				return fmt.Errorf("the data in %s (at %q) is not a UnixFS directory: %s", dagnode.Cid(), paths[i], err)
			}

			var linkResults <-chan unixfs.LinkResult
			if dir == nil {
				linkResults = makeDagNodeLinkResults(req, dagnode)
			} else {
				linkResults = dir.EnumLinksAsync(req.Context)
			}

			output := make([]LsObject, 1)
			outputLinks := make([]LsLink, 1)

			output[0] = newDirectoryHeaderLsObject(paths[i])
			if err = res.Emit(&LsOutput{multipleFolders, output}); err != nil {
				return nil
			}
			for linkResult := range linkResults {
				if linkResult.Err != nil {
					return linkResult.Err
				}
				link := linkResult.Link
				lsLink, err := makeLsLink(req, dserv, resolve, link)
				if err != nil {
					return err
				}
				outputLinks[0] = *lsLink
				output[0] = newDirectoryLinksLsObject(outputLinks)
				if err = res.Emit(&LsOutput{multipleFolders, output}); err != nil {
					return err
				}
			}
			output[0] = newDirectoryFooterLsObject()
			if err = res.Emit(&LsOutput{multipleFolders, output}); err != nil {
				return err
			}
		}
		return nil
	},
	Encoders: cmds.EncoderMap{
		cmds.Text: cmds.MakeEncoder(func(req *cmds.Request, w io.Writer, v interface{}) error {
			headers, _ := req.Options[lsHeadersOptionNameTime].(bool)
			output, ok := v.(*LsOutput)
			if !ok {
				return e.TypeErr(output, v)
			}

			tw := tabwriter.NewWriter(w, 1, 2, 1, ' ', 0)
			for _, object := range output.Objects {
				if object.HasHeader {
					if output.MultipleFolders {
						fmt.Fprintf(tw, "%s:\n", object.Hash)
					}
					if headers {
						fmt.Fprintln(tw, "Hash\tSize\tName")
					}
				}
				if object.HasLinks {
					for _, link := range object.Links {
						if link.Type == unixfs.TDirectory {
							link.Name += "/"
						}

						fmt.Fprintf(tw, "%s\t%v\t%s\n", link.Hash, link.Size, link.Name)
					}
				}
				if object.HasFooter {
					if output.MultipleFolders {
						fmt.Fprintln(tw)
					}
				}
			}
			tw.Flush()
			return nil
		}),
	},
	Type: LsOutput{},
}

func makeDagNodeLinkResults(req *cmds.Request, dagnode ipld.Node) <-chan unixfs.LinkResult {
	linkResults := make(chan unixfs.LinkResult)
	go func() {
		defer close(linkResults)
		for _, l := range dagnode.Links() {
			select {
			case linkResults <- unixfs.LinkResult{
				Link: l,
				Err:  nil,
			}:
			case <-req.Context.Done():
				return
			}
		}
	}()
	return linkResults
}

func newFullDirectoryLsObject(hash string, links []LsLink) LsObject {
	return LsObject{hash, links, true, true, true}
}
func newDirectoryHeaderLsObject(hash string) LsObject {
	return LsObject{hash, nil, true, false, false}
}
func newDirectoryLinksLsObject(links []LsLink) LsObject {
	return LsObject{"", links, false, true, false}
}
func newDirectoryFooterLsObject() LsObject {
	return LsObject{"", nil, false, false, true}
}

func makeLsLink(req *cmds.Request, dserv ipld.DAGService, resolve bool, link *ipld.Link) (*LsLink, error) {
	t := unixfspb.Data_DataType(-1)

	switch link.Cid.Type() {
	case cid.Raw:
		// No need to check with raw leaves
		t = unixfs.TFile
	case cid.DagProtobuf:
		linkNode, err := link.GetNode(req.Context, dserv)
		if err == ipld.ErrNotFound && !resolve {
			// not an error
			linkNode = nil
		} else if err != nil {
			return nil, err
		}

		if pn, ok := linkNode.(*merkledag.ProtoNode); ok {
			d, err := unixfs.FSNodeFromBytes(pn.Data())
			if err != nil {
				return nil, err
			}
			t = d.Type()
		}
	}
	return &LsLink{
		Name: link.Name,
		Hash: link.Cid.String(),
		Size: link.Size,
		Type: t,
	}, nil
}
