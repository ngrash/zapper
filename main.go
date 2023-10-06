package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	g "github.com/AllenDang/giu"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/sahilm/fuzzy"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	mux         sync.RWMutex
	root        topic
	giuStarted  bool
	fuzzyTerm   string
	fuzzyTerms  = make(map[*topic]string)
	fuzzyTopics = make(map[string]*topic)
)

func loop() {
	giuStarted = true

	g.SingleWindow().Layout(
		g.InputText(&fuzzyTerm).Hint("Fuzzy search").Size(g.Auto),
		g.Child().Layout(
			g.TreeTable().
				Columns(
					g.TableColumn("Topic"),
					g.TableColumn("Value"),
				).
				Rows(tableRows()...),
		),
	)
}

func tableRows() []*g.TreeTableRowWidget {
	if fuzzyTerm != "" {
		return fuzzyTableRows()
	} else {
		return rootTableRows()
	}
}

func fuzzyTableRows() []*g.TreeTableRowWidget {
	mux.RLock()
	defer mux.RUnlock()

	var terms []string
	for _, t := range fuzzyTerms {
		terms = append(terms, t)
	}

	fm := fuzzy.Find(fuzzyTerm, terms)
	relevant := make(map[*topic]int)

	for _, m := range fm {
		leaf := fuzzyTopics[m.Str]
		relevant[leaf] = m.Score
		for _, a := range leaf.ancestors() {
			// Mark ancestors as relevant, keeping track of their highest score.
			if s, ok := relevant[a]; ok {
				if m.Score > s {
					relevant[a] = m.Score
				}
			} else {
				relevant[a] = m.Score
			}
		}
	}

	topics := root.filter(relevant)
	var cw []*g.TreeTableRowWidget
	for _, t := range topics {
		cw = append(cw, t.tableRow(relevant))
	}
	return cw
}

func (t *topic) ancestors() []*topic {
	if t.parent == nil {
		return []*topic{}
	} else {
		grandparent := t.parent.ancestors()
		if grandparent != nil {
			return append(grandparent, t.parent)
		}
		return []*topic{t.parent}
	}
}

func rootTableRows() []*g.TreeTableRowWidget {
	mux.RLock()
	keys := make([]string, 0, len(root.children))
	for k := range root.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var cw []*g.TreeTableRowWidget
	for _, k := range keys {
		cw = append(cw, root.children[k].tableRow(nil))
	}
	mux.RUnlock()
	return cw
}

type topic struct {
	parent          *topic
	name            string
	children        map[string]*topic
	last            mqtt.Message
	friendlyPayload *string
}

func (t *topic) tableRow(filter map[*topic]int) *g.TreeTableRowWidget {
	if t.children == nil {
		var value string
		if t.friendlyPayload != nil {
			value = *t.friendlyPayload
		}
		vl := g.Label(value)
		return g.TreeTableRow(t.name,
			vl, g.ContextMenu().MouseButton(g.MouseButtonRight).Layout(
				g.MenuItem("Copy value").OnClick(func() { g.Context.GetPlatform().SetClipboard(value) }),
				g.MenuItem("Copy topic").OnClick(func() { g.Context.GetPlatform().SetClipboard(t.last.Topic()) }),
			),
		).Flags(g.TreeNodeFlagsSpanAvailWidth | g.TreeNodeFlagsLeaf)
	} else {
		if filter == nil {
			keys := make([]string, 0, len(t.children))
			for k := range t.children {
				keys = append(keys, k)
			}
			sort.Strings(keys)

			var cw []*g.TreeTableRowWidget
			for _, k := range keys {
				cw = append(cw, t.children[k].tableRow(filter))
			}
			return g.TreeTableRow(t.name).Flags(g.TreeNodeFlagsSpanAvailWidth | g.TreeNodeFlagsDefaultOpen).Children(cw...)
		} else {
			relevant := t.filter(filter)
			var cw []*g.TreeTableRowWidget
			for _, rc := range relevant {
				cw = append(cw, rc.tableRow(filter))
			}
			return g.TreeTableRow(t.name).Flags(g.TreeNodeFlagsSpanAvailWidth | g.TreeNodeFlagsDefaultOpen).Children(cw...)
		}
	}
}

func (t *topic) filter(filter map[*topic]int) []*topic {
	type kv struct {
		child *topic
		score int
	}
	var relevant []kv
	for _, c := range t.children {
		if score, ok := filter[c]; ok {
			relevant = append(relevant, kv{c, score})
		}
	}
	sort.Slice(relevant, func(i, j int) bool {
		if relevant[i].score == relevant[j].score {
			return strings.Compare(relevant[i].child.name, relevant[j].child.name) == 1
		} else {
			return relevant[i].score > relevant[j].score
		}
	})
	var sorted []*topic
	for _, c := range relevant {
		sorted = append(sorted, c.child)
	}
	return sorted
}

func (t *topic) update(parts []string, msg mqtt.Message) {
	if len(parts) == 0 {
		oldTerm := fmt.Sprintf("%s=%s", msg.Topic(), t.friendlyPayload)
		delete(fuzzyTopics, oldTerm)

		sanitized := sanitize(msg.Payload())

		t.last = msg
		t.friendlyPayload = &sanitized

		newTerm := fmt.Sprintf("%s=%s", msg.Topic(), t.friendlyPayload)
		fuzzyTerms[t] = newTerm
		fuzzyTopics[newTerm] = t
	} else {
		if t.children == nil {
			t.children = make(map[string]*topic)
		}

		name := parts[0]
		rest := parts[1:]

		ct, ok := t.children[name]
		if !ok {
			ct = &topic{parent: t, name: name}
			t.children[name] = ct
		}
		ct.update(rest, msg)
	}
}

func sanitize(payload []byte) string {
	var jsonObject map[string]interface{}
	if err := json.Unmarshal(payload, &jsonObject); err == nil {
		return string(payload)
	}

	if utf8.Valid(payload) {
		possibleString := string(payload)

		if possibleString == "true" {
			return "true"
		} else if possibleString == "false" {
			return "false"
		}

		if _, err := strconv.ParseFloat(possibleString, 64); err == nil {
			return possibleString
		}

		allGraphic := true
		for _, r := range possibleString {
			if !unicode.IsGraphic(r) {
				allGraphic = false
				break
			}
		}
		if allGraphic {
			return fmt.Sprintf("%q", possibleString)
		}
	}

	var floatValue float64
	reader := bytes.NewReader(payload)
	if err := binary.Read(reader, binary.LittleEndian, &floatValue); err == nil {
		// Checking if it's a valid float (not NaN or Inf)
		if !math.IsNaN(floatValue) && !math.IsInf(floatValue, 0) {
			return fmt.Sprintf("%f", floatValue)
		}
	}

	_, _ = reader.Seek(0, 0) // Resetting reader
	var intValue int32
	if err := binary.Read(reader, binary.LittleEndian, &intValue); err == nil {
		return fmt.Sprintf("%d", intValue)
	}

	return fmt.Sprintf("%#x", payload)
}

func defaultHandler(_ mqtt.Client, msg mqtt.Message) {
	//log.Println("received", msg.Topic())
	parts := strings.Split(msg.Topic(), "/")

	mux.Lock()
	root.update(parts, msg)
	mux.Unlock()

	if giuStarted {
		g.Update()
	}
}

var (
	brokerFlag   = flag.String("broker", "tcp://test.mosquitto.org:1883", "broker to explore (scheme://host:port)")
	clientIDFlag = flag.String("client-id", "", "client ID, leave empty to generate one")
)

func main() {
	flag.Parse()

	var clientID string
	if *clientIDFlag != "" {
		clientID = *clientIDFlag
	} else {
		clientID = fmt.Sprintf("zapper-%s", randomClientID())
	}

	opts := mqtt.NewClientOptions().AddBroker(*brokerFlag).SetClientID(clientID)
	opts.SetKeepAlive(2 * time.Second)
	opts.SetPingTimeout(2 * time.Second)
	opts.SetDefaultPublishHandler(defaultHandler)
	opts.SetCleanSession(true)

	c := mqtt.NewClient(opts)
	if t := c.Connect(); t.Wait() && t.Error() != nil {
		log.Fatal(t.Error())
	}

	if t := c.Subscribe("#", 0, nil); t.Wait() && t.Error() != nil {
		log.Fatal(t.Error())
	}

	wnd := g.NewMasterWindow(clientID, 800, 800, 0)
	wnd.Run(loop)
}

func randomClientID() string {
	b := make([]byte, 5)
	_, err := rand.Read(b)
	if err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(b)
}
