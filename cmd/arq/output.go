// Copyright Armada Contributors

package main

import (
	"fmt"
	"io"

	"github.com/armadakv/armada/armadapb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func writeJSON(w io.Writer, message proto.Message) error {
	data, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(message)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

func writeRangeSimple(w io.Writer, response *armadapb.RangeResponse, keysOnly, countOnly, valueOnly bool) error {
	if countOnly {
		_, err := fmt.Fprintln(w, response.GetCount())
		return err
	}
	for _, kv := range response.GetKvs() {
		if !valueOnly {
			if _, err := fmt.Fprintln(w, string(kv.GetKey())); err != nil {
				return err
			}
		}
		if !keysOnly {
			if _, err := fmt.Fprintln(w, string(kv.GetValue())); err != nil {
				return err
			}
		}
	}
	return nil
}

func writePreviousKVs(w io.Writer, kvs []*armadapb.KeyValue) error {
	for _, kv := range kvs {
		if _, err := fmt.Fprintln(w, string(kv.GetKey())); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, string(kv.GetValue())); err != nil {
			return err
		}
	}
	return nil
}
