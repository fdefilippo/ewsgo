// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestCalendarSearchItemUsesActiveAlarmAndAppointmentRecurrence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		role           core.SpecialFolderKind
		element        string
		active         bool
		wantRecurrence core.Value
	}{
		{"dismissed appointment", core.CalendarFolder, "appt", false, core.Bool(true)},
		{"active appointment", core.CalendarFolder, "appt", true, core.Bool(true)},
		{"active recurring task", core.TasksFolder, "task", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit := element(mailNS, tc.element, "")
			for name, value := range map[string]string{
				"name": "Test", "md": "1700000000", "ms": "31", "rev": "29", "alarm": "1", "recur": "1",
			} {
				hit = attr(hit, name, value)
			}
			if tc.active {
				hit.Children = append(hit.Children, attr(element(mailNS, "alarmData", ""), "nextAlarm", "1700000300123"))
			}
			object, err := calendarSearchItem(hit, "folder", "item", tc.role)
			if err != nil {
				t.Fatal(err)
			}
			p := object.Properties
			if p[core.ReminderSet] != core.Bool(tc.active) || p[core.AppointmentRecurring] != tc.wantRecurrence {
				t.Fatal("unexpected reminder or recurrence state")
			}
			var signal core.Value
			if tc.active {
				signal = core.Timestamp(time.UnixMilli(1700000300123).UTC())
			}
			if p[core.ReminderSignalTime] != signal {
				t.Fatal("unexpected reminder signal time")
			}
			if object.ID != "item" || object.Parent != "folder" || object.Revision != 31 || len(object.MIME) != 0 {
				t.Fatal("unexpected calendar search identity")
			}
		})
	}
}

func TestCalendarSearchUsesCompleteStablePages(t *testing.T) {
	const size = 257
	const owner = "00000000-0000-0000-0000-000000000001"
	const mountID = "88"
	for _, shared := range []bool{false, true} {
		for _, scenario := range []string{"complete", "duplicate", "sequence change", "missing page", "truncated final page", "wrong parent"} {
			t.Run(fmt.Sprintf("shared=%t/%s", shared, scenario), func(t *testing.T) {
				folder, parent, searchFolder := core.ID("f:10"), "10", "10"
				prefix := ""
				if shared {
					folder, parent, searchFolder = core.ID(core.SharedGroupwareFolderIDPrefix+mountID), owner+":10", mountID
					prefix = owner + ":"
				}
				scans, markerReads := 0, 0
				client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					var request node
					if err := xml.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error("cannot decode fixture request", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					req := request.child(soapNS, "Body").Children[0]
					switch req.XMLName.Local {
					case "GetFolderRequest":
						if f := req.child(mailNS, "folder"); shared && f != nil && f.get("l") == mountID {
							envelope(w, fmt.Sprintf(`<GetFolderResponse xmlns="urn:zimbraMail"><link id="%s" l="1" zid="%s" rid="10" view="appointment" perm="r"/></GetFolderResponse>`, mountID, owner))
							return
						}
						markerReads++
						seq := 10
						if scenario == "sequence change" && markerReads > 1 {
							seq++
						}
						fields := fmt.Sprintf(`view="appointment" n="%d" i4ms="%d"`, size, seq)
						if shared {
							envelope(w, fmt.Sprintf(`<GetFolderResponse xmlns="urn:zimbraMail"><folder id="%s" perm="r" %s/></GetFolderResponse>`, parent, fields))
						} else {
							envelope(w, fmt.Sprintf(`<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="10" l="1" %s/></folder></GetFolderResponse>`, fields))
						}
					case "SearchRequest":
						scans++
						offset, _ := strconv.Atoi(req.get("offset"))
						point := req.child(mailNS, "query").Text == "inid:10 item:1256"
						if req.get("types") != "appointment" || req.get("sortBy") != "idAsc" || req.get("limit") != "100" ||
							(!point && req.child(mailNS, "query").Text != "inid:"+searchFolder) {
							t.Error("unexpected calendar search scope")
						}
						low, end := offset, min(offset+100, size)
						if point {
							low, end = size-1, size
						}
						if scenario == "missing page" && offset == 100 {
							end = offset
						}
						var rows strings.Builder
						for i := low; i < end; i++ {
							class := "PUB"
							if shared && i == 1 {
								class = "PRI"
							}
							id, location := 1000+i, parent
							if scenario == "duplicate" && i == 100 {
								id--
							}
							if scenario == "wrong parent" && i == 100 {
								location = "2"
							}
							fmt.Fprintf(&rows, `<appt id="%s%d" l="%s" ms="12" rev="11" md="1795757815" name="Calendar event" class="%s"/>`, prefix, id, location, class)
						}
						more := end < size && !(scenario == "truncated final page" && offset == 100)
						envelope(w, fmt.Sprintf(`<SearchResponse xmlns="urn:zimbraMail" offset="%d" more="%t" sortBy="idAsc">%s</SearchResponse>`, offset, more, rows.String()))
					default:
						t.Error("unexpected operation", req.XMLName.Local)
						w.WriteHeader(http.StatusBadRequest)
					}
				}, Config{})
				box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}}
				items, err := box.ReadCalendarSearchItems(t.Context(), folder, core.CalendarFolder)
				if scenario != "complete" {
					if err == nil || items != nil {
						t.Fatalf("invalid scan produced %d rows", len(items))
					}
					return
				}
				want := size
				if shared {
					want-- // Private entry omitted without losing the next page's offset.
				}
				if err != nil || len(items) != want || scans != 3 {
					t.Fatalf("complete list: items=%d scans=%d error=%v", len(items), scans, err)
				}
				observation, err := box.ObserveFolderContents(t.Context(), folder, nil)
				if err != nil || observation.MessageCount != uint64(want) || len(observation.MessageIDs) != want {
					t.Fatalf("complete calendar observation: count=%d error=%v", observation.MessageCount, err)
				}
				before := scans
				for _, malformed := range []core.ID{
					core.AppointmentIDPrefix,
					core.ID(core.AppointmentIDPrefix + prefix + "01256"),
					core.ID(core.AppointmentIDPrefix + prefix + "invalid"),
				} {
					if _, err := box.GetCalendarAppointment(t.Context(), folder, malformed); !errors.Is(err, core.ErrNotFound) {
						t.Fatalf("invalid item ID returned %v", err)
					}
				}
				if scans != before {
					t.Fatal("invalid item ID caused a calendar search")
				}
				point, err := box.readCalendarSearchItems(t.Context(), folder, core.CalendarFolder, prefix+"1256")
				wantPages := 1
				if shared {
					wantPages = 3
				}
				if err != nil || len(point) != 1 || point[0].ID != core.ID("a:"+prefix+"1256") || scans-before != wantPages {
					t.Fatalf("point lookup: items=%d scans=%d error=%v", len(point), scans-before, err)
				}
			})
		}
	}
}
