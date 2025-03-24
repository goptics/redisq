package redisq

import "encoding/json"

type event struct {
	Action  string `json:"action"`
	Message []byte `json:"message"`
}

func (e *event) Json() ([]byte, error) {
	json, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}

	return json, nil
}

func parseToEvent(data []byte) (event, error) {
	var e event
	if err := json.Unmarshal(data, &e); err != nil {
		return event{}, err
	}

	return e, nil
}
