package trace

type Tracer interface {
	ReceivedFromChannel(name string)
	SendingToChannel(name string)
	SentToChannel(name string)
	Event(message string)
	WorkerEvent(message string, worker int)
	IterationEvent(message string, iteration uint64)
}
