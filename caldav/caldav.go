package caldav

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
	"github.com/emersion/hydroxide/protonmail"
)

var errNotFound = errors.New("caldav: not found")

type backend struct {
	c           *protonmail.Client
	cache       map[string]*protonmail.Calendar
	eventCache  map[string]map[string]*protonmail.CalendarEvent // calendarID -> eventID -> event
	locker      sync.Mutex
	privateKeys openpgp.EntityList
}

func (b *backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return "/", nil
}

func (b *backend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	return "/calendars", nil
}

func (b *backend) CreateCalendar(ctx context.Context, cal *caldav.Calendar) error {
	return webdav.NewHTTPError(http.StatusForbidden, errors.New("cannot create new calendar"))
}

func (b *backend) DeleteCalendar(ctx context.Context, path string) error {
	return webdav.NewHTTPError(http.StatusForbidden, errors.New("cannot delete calendar"))
}

func (b *backend) ListCalendars(ctx context.Context) ([]caldav.Calendar, error) {
	b.locker.Lock()
	defer b.locker.Unlock()

	if b.cache == nil {
		calendars, err := b.c.ListCalendars(0, 0)
		if err != nil {
			return nil, err
		}
		b.cache = make(map[string]*protonmail.Calendar)
		for _, cal := range calendars {
			b.cache[cal.ID] = cal
		}
	}

	var result []caldav.Calendar
	for _, cal := range b.cache {
		result = append(result, caldav.Calendar{
			Path:            "/calendars/" + cal.ID + "/",
			Name:            cal.Name,
			Description:     cal.Description,
			MaxResourceSize: 10 * 1024 * 1024, // 10MB
		})
	}

	return result, nil
}

func (b *backend) GetCalendar(ctx context.Context, path string) (*caldav.Calendar, error) {
	calID, err := parseCalendarPath(path)
	if err != nil {
		return nil, err
	}

	calendars, err := b.ListCalendars(ctx)
	if err != nil {
		return nil, err
	}

	for _, cal := range calendars {
		if strings.TrimSuffix(cal.Path, "/") == "/calendars/"+calID {
			return &cal, nil
		}
	}

	return nil, webdav.NewHTTPError(http.StatusNotFound, errors.New("calendar not found"))
}

func parseCalendarPath(p string) (string, error) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 2 || parts[0] != "calendars" {
		return "", errNotFound
	}
	return parts[1], nil
}

func parseObjectPath(p string) (calendarID, eventID string, err error) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 3 || parts[0] != "calendars" {
		return "", "", errNotFound
	}
	ext := path.Ext(parts[2])
	if ext != ".ics" {
		return "", "", errNotFound
	}
	return parts[1], strings.TrimSuffix(parts[2], ext), nil
}

func formatObjectPath(calendarID, eventID string) string {
	return "/calendars/" + calendarID + "/" + eventID + ".ics"
}

func (b *backend) toCalendarObject(event *protonmail.CalendarEvent, cal *protonmail.Calendar) (*caldav.CalendarObject, error) {
	// Create a basic iCal event
	calObj := &caldav.CalendarObject{
		Path:    formatObjectPath(cal.ID, event.ID),
		ModTime: time.Unix(event.CreateTime.Unix(), 0),
		ETag:    fmt.Sprintf("%x", event.LastEditTime),
	}

	// Parse the event data from CalendarEventCard
	for _, card := range event.PersonalEvent {
		if card.Data != "" {
			decoded, err := ical.NewDecoder(strings.NewReader(card.Data)).Decode()
			if err != nil {
				continue
			}
			calObj.Data = decoded
			break
		}
	}

	if calObj.Data == nil {
		// Create a basic calendar if no data
		calObj.Data = ical.NewCalendar()
	}

	return calObj, nil
}

func (b *backend) GetCalendarObject(ctx context.Context, path string, req *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	calID, eventID, err := parseObjectPath(path)
	if err != nil {
		return nil, err
	}

	// Get calendar
	cal, ok := b.cache[calID]
	if !ok {
		calendars, err := b.c.ListCalendars(0, 0)
		if err != nil {
			return nil, err
		}
		for _, c := range calendars {
			if c.ID == calID {
				cal = c
				break
			}
		}
		if cal == nil {
			return nil, errNotFound
		}
	}

	// Get event
	filter := &protonmail.CalendarEventFilter{
		Start:    time.Now().AddDate(-1, 0, 0).Unix(),
		End:      time.Now().AddDate(1, 0, 0).Unix(),
		Timezone: "UTC",
		Page:     0,
	}

	events, err := b.c.ListCalendarEvents(calID, filter)
	if err != nil {
		return nil, err
	}

	for _, event := range events {
		if event.ID == eventID {
			return b.toCalendarObject(event, cal)
		}
	}

	return nil, errNotFound
}

func (b *backend) ListCalendarObjects(ctx context.Context, path string, req *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	calID, err := parseCalendarPath(path)
	if err != nil {
		return nil, err
	}

	// Get calendar
	cal, ok := b.cache[calID]
	if !ok {
		calendars, err := b.c.ListCalendars(0, 0)
		if err != nil {
			return nil, err
		}
		for _, c := range calendars {
			if c.ID == calID {
				cal = c
				break
			}
		}
		if cal == nil {
			return nil, errNotFound
		}
	}

	// Get events
	filter := &protonmail.CalendarEventFilter{
		Start:    time.Now().AddDate(-1, 0, 0).Unix(),
		End:      time.Now().AddDate(1, 0, 0).Unix(),
		Timezone: "UTC",
		Page:     0,
	}

	events, err := b.c.ListCalendarEvents(calID, filter)
	if err != nil {
		return nil, err
	}

	var result []caldav.CalendarObject
	for _, event := range events {
		obj, err := b.toCalendarObject(event, cal)
		if err != nil {
			continue
		}
		result = append(result, *obj)
	}

	return result, nil
}

func (b *backend) QueryCalendarObjects(ctx context.Context, path string, query *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	req := caldav.CalendarCompRequest{AllProps: true}
	if query != nil {
		req = query.CompRequest
	}
	return b.ListCalendarObjects(ctx, path, &req)
}

func (b *backend) PutCalendarObject(ctx context.Context, path string, cal *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	// TODO: Implement creating/updating calendar events
	return nil, webdav.NewHTTPError(http.StatusNotImplemented, errors.New("calendar event creation not yet implemented"))
}

func (b *backend) DeleteCalendarObject(ctx context.Context, path string) error {
	// TODO: Implement deleting calendar events
	return webdav.NewHTTPError(http.StatusNotImplemented, errors.New("calendar event deletion not yet implemented"))
}

func NewHandler(c *protonmail.Client, privateKeys openpgp.EntityList, events <-chan *protonmail.Event) http.Handler {
	if len(privateKeys) == 0 {
		panic("hydroxide/caldav: no private key available")
	}

	b := &backend{
		c:           c,
		cache:       make(map[string]*protonmail.Calendar),
		eventCache:  make(map[string]map[string]*protonmail.CalendarEvent),
		privateKeys: privateKeys,
	}

	return &caldav.Handler{Backend: b}
}