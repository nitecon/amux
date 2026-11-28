package queues

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
)

type SQSQueue struct {
	client    *sqs.SQS
	queueURL  string
	Processor MessageProcessor
	done      <-chan struct{}
}

func (q *SQSQueue) GetCurrentList() map[string]string {
	//TODO implement me
	panic("implement me")
}

func NewSQSQueue(queueURL string, processor MessageProcessor) (*SQSQueue, error) {
	return newSQSQueue(context.Background(), queueURL, processor)
}

func newSQSQueue(ctx context.Context, queueURL string, processor MessageProcessor) (*SQSQueue, error) {
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String("us-west-2"),
	})
	if err != nil {
		return nil, err
	}
	svc := sqs.New(sess)
	return &SQSQueue{
		Processor: processor,
		done:      ctx.Done(),
		client:    svc,
		queueURL:  queueURL,
	}, nil
}

func (q *SQSQueue) ConnectResponse() {

}

func (q *SQSQueue) Enqueue(ctx context.Context, id string, payload string, ttl int64) error {
	_, err := q.client.SendMessageWithContext(ctx, &sqs.SendMessageInput{
		QueueUrl:    &q.queueURL,
		MessageBody: aws.String(payload),
		MessageAttributes: map[string]*sqs.MessageAttributeValue{
			"ID": {
				DataType:    aws.String("String"),
				StringValue: aws.String(id),
			},
		},
		MessageDeduplicationId: aws.String(fmt.Sprintf("%s-%d", id, time.Now().Unix())),
		MessageGroupId:         aws.String(id),
	})
	return err
}

func (q *SQSQueue) Dequeue(ctx context.Context, ids []string, processor MessageProcessor) (map[string]string, error) {
	result, err := q.client.ReceiveMessageWithContext(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            &q.queueURL,
		MaxNumberOfMessages: aws.Int64(10),
		WaitTimeSeconds:     aws.Int64(10),
		MessageAttributeNames: aws.StringSlice([]string{
			"ID",
		}),
	})
	if err != nil {
		return nil, err
	}

	var messages map[string]string
	for _, msg := range result.Messages {
		if *msg.Body != "" {
			messages["id"] = *msg.MessageAttributes["ID"].StringValue
		}
	}

	return messages, nil
}

func (q *SQSQueue) Acknowledge(ctx context.Context, id string, message string) error {
	// Acknowledge by deleting the message from the queue
	_, err := q.client.DeleteMessageWithContext(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      &q.queueURL,
		ReceiptHandle: aws.String(message),
	})
	return err
}

func (q *SQSQueue) Done() <-chan struct{} { return q.done }
