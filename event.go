package redisq

import "encoding/json"

type Event struct {
	Action  string `json:"action"`
	Message []byte `json:"message"`
}

func (e *Event) Json() ([]byte, error) {
	json, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}

	return json, nil
}

func parseToEvent(data []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return Event{}, err
	}

	return event, nil
}
