package zerocopy

type ZeroCopyBuffer struct {
	Buffer []byte
}

func (c *ZeroCopyBuffer) TryGetRequestedSizeBuffer(newBuffer *[]byte, requestedSize int) []byte {
	if len(c.Buffer) == 0 {
		if requestedSize > len(*newBuffer) {
			c.Buffer = append(c.Buffer, *newBuffer...)
			*newBuffer = nil
			return nil
		}
		// can use buffer directly without copying
		res := (*newBuffer)[:requestedSize]
		(*newBuffer) = (*newBuffer)[requestedSize:]
		return res
	}
	c.Buffer = append(c.Buffer, *newBuffer...)
	*newBuffer = nil
	if len(c.Buffer) < requestedSize {
		return nil
	}
	res := c.Buffer[:requestedSize]
	c.Buffer = c.Buffer[requestedSize:]
	return res
}
