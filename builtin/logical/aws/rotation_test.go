// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package aws

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/iam/iamiface"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/awsutil"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/helper/testhelpers"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/hashicorp/vault/sdk/queue"
	"github.com/hashicorp/vault/vault"
	"github.com/stretchr/testify/require"
)

// TestRotation verifies that the rotation code and priority queue correctly selects and rotates credentials
// for static secrets.
func TestRotation(t *testing.T) {
	bgCTX := context.Background()

	type credToInsert struct {
		config staticRoleEntry // role configuration from a normal createRole request
		age    time.Duration   // how old the cred should be - if this is longer than the config.RotationPeriod,
		// the cred is 'pre-expired'

		changed bool // whether we expect the cred to change - this is technically redundant to a comparison between
		// rotationPeriod and age.
	}

	// due to a limitation with the mockIAM implementation, any cred you want to rotate must have
	// username jane-doe and userid unique-id, since we can only pre-can one exact response to GetUser
	cases := []struct {
		name  string
		creds []credToInsert
	}{
		{
			name: "refresh one",
			creds: []credToInsert{
				{
					config: staticRoleEntry{
						Name:           "test",
						Username:       "jane-doe",
						ID:             "unique-id",
						RotationPeriod: 2 * time.Second,
					},
					age:     5 * time.Second,
					changed: true,
				},
			},
		},
		{
			name: "refresh none",
			creds: []credToInsert{
				{
					config: staticRoleEntry{
						Name:           "test",
						Username:       "jane-doe",
						ID:             "unique-id",
						RotationPeriod: 1 * time.Minute,
					},
					age:     5 * time.Second,
					changed: false,
				},
			},
		},
		{
			name: "refresh one of two",
			creds: []credToInsert{
				{
					config: staticRoleEntry{
						Name:           "toast",
						Username:       "john-doe",
						ID:             "other-id",
						RotationPeriod: 1 * time.Minute,
					},
					age:     5 * time.Second,
					changed: false,
				},
				{
					config: staticRoleEntry{
						Name:           "test",
						Username:       "jane-doe",
						ID:             "unique-id",
						RotationPeriod: 1 * time.Second,
					},
					age:     5 * time.Second,
					changed: true,
				},
			},
		},
		{
			name:  "no creds to rotate",
			creds: []credToInsert{},
		},
	}

	ak := "long-access-key-id"
	oldSecret := "abcdefghijklmnopqrstuvwxyz"
	newSecret := "zyxwvutsrqponmlkjihgfedcba"

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config := logical.TestBackendConfig()
			config.StorageView = &logical.InmemStorage{}

			b := Backend(config)

			expirations := make([]*time.Time, len(c.creds))
			// insert all our creds
			for i, cred := range c.creds {

				// all the creds will be the same for every user, but that's okay
				// since what we care about is whether they changed on a single-user basis.
				miam, err := awsutil.NewMockIAM(
					// blank list for existing user
					awsutil.WithListAccessKeysOutput(&iam.ListAccessKeysOutput{
						AccessKeyMetadata: []*iam.AccessKeyMetadata{
							{},
						},
					}),
					// initial key to store
					awsutil.WithCreateAccessKeyOutput(&iam.CreateAccessKeyOutput{
						AccessKey: &iam.AccessKey{
							AccessKeyId:     aws.String(ak),
							SecretAccessKey: aws.String(oldSecret),
						},
					}),
					awsutil.WithGetUserOutput(&iam.GetUserOutput{
						User: &iam.User{
							UserId:   aws.String(cred.config.ID),
							UserName: aws.String(cred.config.Username),
						},
					}),
				)(nil)
				if err != nil {
					t.Fatalf("couldn't initialze mock IAM handler: %s", err)
				}

				// Used to override the IAM client creation to return the mocked client
				b.nonCachedClientIAMFunc = func(ctx context.Context, storage logical.Storage, logger hclog.Logger, entry *staticRoleEntry) (iamiface.IAMAPI, error) {
					if entry.Username == cred.config.Username && entry.ID == cred.config.ID {
						return miam, nil
					}
					return nil, fmt.Errorf("unexpected IAM client creation for user %q", entry.Username)
				}

				c, err := b.createCredential(bgCTX, config.StorageView, cred.config, true)
				if err != nil {
					t.Fatalf("couldn't insert credential %d: %s", i, err)
				}

				expirations[i] = c.Expiration
				item := &queue.Item{
					Key:      cred.config.Name,
					Value:    cred.config,
					Priority: time.Now().Add(-1 * cred.age).Add(cred.config.RotationPeriod).Unix(),
				}
				err = b.credRotationQueue.Push(item)
				if err != nil {
					t.Fatalf("couldn't push item onto queue: %s", err)
				}
			}

			// update aws responses, same argument for why it's okay every cred will be the same
			miam, err := awsutil.NewMockIAM(
				// old key
				awsutil.WithListAccessKeysOutput(&iam.ListAccessKeysOutput{
					AccessKeyMetadata: []*iam.AccessKeyMetadata{
						{
							AccessKeyId: aws.String(ak),
						},
					},
				}),
				// new key
				awsutil.WithCreateAccessKeyOutput(&iam.CreateAccessKeyOutput{
					AccessKey: &iam.AccessKey{
						AccessKeyId:     aws.String(ak),
						SecretAccessKey: aws.String(newSecret),
					},
				}),
				awsutil.WithGetUserOutput(&iam.GetUserOutput{
					User: &iam.User{
						UserId:   aws.String("unique-id"),
						UserName: aws.String("jane-doe"),
					},
				}),
			)(nil)
			if err != nil {
				t.Fatalf("couldn't initialze mock IAM handler: %s", err)
			}

			// Set the IAM mock client to be used in the rotation
			b.nonCachedClientIAMFunc = func(ctx context.Context, storage logical.Storage, logger hclog.Logger, entry *staticRoleEntry) (iamiface.IAMAPI, error) {
				return miam, nil
			}

			req := &logical.Request{
				Storage: config.StorageView,
			}
			err = b.rotateExpiredStaticCreds(bgCTX, req)
			if err != nil {
				t.Fatalf("got an error rotating credentials: %s", err)
			}

			// check our credentials
			for i, cred := range c.creds {
				entry, err := config.StorageView.Get(bgCTX, formatCredsStoragePath(cred.config.Name))
				if err != nil {
					t.Fatalf("got an error retrieving credentials %d", i)
				}
				var out awsCredentials
				err = entry.DecodeJSON(&out)
				if err != nil {
					t.Fatalf("could not unmarshal storage view entry for cred %d to an aws credential: %s", i, err)
				}

				if cred.changed {
					require.Equal(t, out.SecretAccessKey, newSecret, "expected the key for cred %d to have changed, but it hasn't", i)
					require.NotEqual(t, out.Expiration.UTC(), expirations[i].UTC(), "expected the expiration for cred %d to have changed, but it hasn't", i)
				} else {
					require.Equal(t, out.SecretAccessKey, oldSecret, "expected the key for cred %d to have stayed the same, but it changed", i)
					require.Equal(t, out.Expiration.UTC(), expirations[i].UTC(), "expected the expiration for cred %d to have changed, but it hasn't", i)
				}
			}
		})
	}
}

type fakeIAM struct {
	iamiface.IAMAPI
	delReqs []*iam.DeleteAccessKeyInput
}

func (f *fakeIAM) DeleteAccessKey(r *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	f.delReqs = append(f.delReqs, r)
	return f.IAMAPI.DeleteAccessKey(r)
}

// TestCreateCredential verifies that credential creation firstly only deletes credentials if it needs to (i.e., two
// or more credentials on IAM), and secondly correctly deletes the oldest one.
func TestCreateCredential(t *testing.T) {
	cases := []struct {
		name       string
		username   string
		id         string
		deletedKey string
		opts       []awsutil.MockIAMOption
	}{
		{
			name:     "zero keys",
			username: "jane-doe",
			id:       "unique-id",
			opts: []awsutil.MockIAMOption{
				awsutil.WithListAccessKeysOutput(&iam.ListAccessKeysOutput{
					AccessKeyMetadata: []*iam.AccessKeyMetadata{},
				}),
				// delete should _not_ be called
				awsutil.WithDeleteAccessKeyError(errors.New("should not have been called")),
				awsutil.WithCreateAccessKeyOutput(&iam.CreateAccessKeyOutput{
					AccessKey: &iam.AccessKey{
						AccessKeyId:     aws.String("key"),
						SecretAccessKey: aws.String("itsasecret"),
					},
				}),
				awsutil.WithGetUserOutput(&iam.GetUserOutput{
					User: &iam.User{
						UserId:   aws.String("unique-id"),
						UserName: aws.String("jane-doe"),
					},
				}),
			},
		},
		{
			name:     "one key",
			username: "jane-doe",
			id:       "unique-id",
			opts: []awsutil.MockIAMOption{
				awsutil.WithListAccessKeysOutput(&iam.ListAccessKeysOutput{
					AccessKeyMetadata: []*iam.AccessKeyMetadata{
						{AccessKeyId: aws.String("foo"), CreateDate: aws.Time(time.Now())},
					},
				}),
				// delete should _not_ be called
				awsutil.WithDeleteAccessKeyError(errors.New("should not have been called")),
				awsutil.WithCreateAccessKeyOutput(&iam.CreateAccessKeyOutput{
					AccessKey: &iam.AccessKey{
						AccessKeyId:     aws.String("key"),
						SecretAccessKey: aws.String("itsasecret"),
					},
				}),
				awsutil.WithGetUserOutput(&iam.GetUserOutput{
					User: &iam.User{
						UserId:   aws.String("unique-id"),
						UserName: aws.String("jane-doe"),
					},
				}),
			},
		},
		{
			name:       "two keys",
			username:   "jane-doe",
			id:         "unique-id",
			deletedKey: "foo",
			opts: []awsutil.MockIAMOption{
				awsutil.WithListAccessKeysOutput(&iam.ListAccessKeysOutput{
					AccessKeyMetadata: []*iam.AccessKeyMetadata{
						{AccessKeyId: aws.String("foo"), CreateDate: aws.Time(time.Time{})},
						{AccessKeyId: aws.String("bar"), CreateDate: aws.Time(time.Now())},
					},
				}),
				awsutil.WithCreateAccessKeyOutput(&iam.CreateAccessKeyOutput{
					AccessKey: &iam.AccessKey{
						AccessKeyId:     aws.String("key"),
						SecretAccessKey: aws.String("itsasecret"),
					},
				}),
				awsutil.WithGetUserOutput(&iam.GetUserOutput{
					User: &iam.User{
						UserId:   aws.String("unique-id"),
						UserName: aws.String("jane-doe"),
					},
				}),
			},
		},
	}

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			miam, err := awsutil.NewMockIAM(
				c.opts...,
			)(nil)
			if err != nil {
				t.Fatal(err)
			}
			fiam := &fakeIAM{
				IAMAPI: miam,
			}

			b := Backend(config)

			b.nonCachedClientIAMFunc = func(ctx context.Context, s logical.Storage, logger hclog.Logger, entry *staticRoleEntry) (iamiface.IAMAPI, error) {
				return fiam, nil
			}

			_, err = b.createCredential(context.Background(), config.StorageView, staticRoleEntry{Username: c.username, ID: c.id}, true)
			if err != nil {
				t.Fatalf("got an error we didn't expect: %q", err)
			}

			if c.deletedKey != "" {
				if len(fiam.delReqs) != 1 {
					t.Fatalf("called the wrong number of deletes (called %d deletes)", len(fiam.delReqs))
				}
				actualKey := *fiam.delReqs[0].AccessKeyId
				if c.deletedKey != actualKey {
					t.Fatalf("we deleted the wrong key: %q instead of %q", actualKey, c.deletedKey)
				}
			}
		})
	}
}

// TestRequeueOnError verifies that in the case of an error, the entry will still be in the queue for later rotation
func TestRequeueOnError(t *testing.T) {
	bgCTX := context.Background()

	cred := staticRoleEntry{
		Name:           "test",
		Username:       "jane-doe",
		RotationPeriod: 30 * time.Minute,
	}

	ak := "long-access-key-id"
	oldSecret := "abcdefghijklmnopqrstuvwxyz"

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}

	b := Backend(config)

	// go through the process of adding a key
	miam, err := awsutil.NewMockIAM(
		awsutil.WithListAccessKeysOutput(&iam.ListAccessKeysOutput{
			AccessKeyMetadata: []*iam.AccessKeyMetadata{
				{},
			},
		}),
		// initial key to store
		awsutil.WithCreateAccessKeyOutput(&iam.CreateAccessKeyOutput{
			AccessKey: &iam.AccessKey{
				AccessKeyId:     aws.String(ak),
				SecretAccessKey: aws.String(oldSecret),
			},
		}),
		awsutil.WithGetUserOutput(&iam.GetUserOutput{
			User: &iam.User{
				UserId:   aws.String(cred.ID),
				UserName: aws.String(cred.Username),
			},
		}),
	)(nil)
	if err != nil {
		t.Fail()
	}

	// Used to override the IAM real client creation to return the mocked client
	b.nonCachedClientIAMFunc = func(ctx context.Context, s logical.Storage, logger hclog.Logger, entry *staticRoleEntry) (iamiface.IAMAPI, error) {
		return miam, nil
	}

	_, err = b.createCredential(bgCTX, config.StorageView, cred, true)
	if err != nil {
		t.Fatalf("couldn't insert credential: %s", err)
	}

	// put the cred in the queue but age it out
	item := &queue.Item{
		Key:      cred.Name,
		Value:    cred,
		Priority: time.Now().Add(-10 * time.Minute).Unix(),
	}
	err = b.credRotationQueue.Push(item)
	if err != nil {
		t.Fatalf("couldn't push item onto queue: %s", err)
	}

	// update the mock iam with the next requests
	miam, err = awsutil.NewMockIAM(
		awsutil.WithGetUserError(errors.New("oh no")),
	)(nil)
	if err != nil {
		t.Fatalf("couldn't initialize the mock iam: %s", err)
	}
	b.nonCachedClientIAMFunc = func(ctx context.Context, s logical.Storage, logger hclog.Logger, entry *staticRoleEntry) (iamiface.IAMAPI, error) {
		return miam, nil
	}

	// now rotate, but it will fail
	r, e := b.rotateCredential(bgCTX, config.StorageView)
	if !r {
		t.Fatalf("rotate credential should return true in this case, but it didn't")
	}
	if e == nil {
		t.Fatalf("we expected an error when rotating a credential, but didn't get one")
	}
	// the queue should be updated though
	i, e := b.credRotationQueue.PopByKey(cred.Name)
	if err != nil {
		t.Fatalf("queue error: %s", e)
	}
	delta := time.Now().Add(10*time.Second).Unix() - i.Priority
	if delta < -5 || delta > 5 {
		t.Fatalf("priority should be within 5 seconds of our backoff interval")
	}
}

type mockIAM struct {
	iamiface.IAMAPI
	// mapping username -> number of times CreateAccessKey has been queried
	// for this user
	newKeys map[string]int
	l       sync.Mutex
}

func (m *mockIAM) GetUser(input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return &iam.GetUserOutput{User: &iam.User{UserId: aws.String(""), UserName: input.UserName}}, nil
}

func (m *mockIAM) ListAccessKeys(input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	return &iam.ListAccessKeysOutput{
		AccessKeyMetadata: []*iam.AccessKeyMetadata{
			{
				AccessKeyId: aws.String(fmt.Sprintf("%s-key", *input.UserName)),
			},
		},
	}, nil
}

func (m *mockIAM) CreateAccessKey(input *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	m.l.Lock()
	defer m.l.Unlock()
	m.newKeys[*input.UserName]++
	count := m.newKeys[*input.UserName]
	return &iam.CreateAccessKeyOutput{
		AccessKey: &iam.AccessKey{
			AccessKeyId:     aws.String(fmt.Sprintf("%s-key", *input.UserName)),
			SecretAccessKey: aws.String(fmt.Sprintf("%s-%d", *input.UserName, count)),
		},
	}, nil
}

// Test_RotationQueueInitialized creates a 2 node cluster and sets up the AWS
// credentials backend. The test creates 3 sets of static credentials. Two of
// those have a low rotation period and should get rotated during the test. The
// third has a high rotation period and should not be rotated. The test verifies
// that the correct secrets are rotated, then transfers leadership to the other
// node. The test verifies that credentials are once again rotated on the new
// active node.
func Test_RotationQueueInitialized(t *testing.T) {
	mockClient := &mockIAM{
		newKeys: make(map[string]int),
	}
	coreConfig := &vault.CoreConfig{
		LogicalBackends: map[string]logical.Factory{
			"aws": func(ctx context.Context, config *logical.BackendConfig) (logical.Backend, error) {
				b := Backend(config)
				b.iamClient = mockClient
				b.minAllowableRotationPeriod = 1 * time.Second

				// Used to override the IAM real client creation to return the mocked client
				b.nonCachedClientIAMFunc = func(ctx context.Context, storage logical.Storage, logger hclog.Logger, entry *staticRoleEntry) (iamiface.IAMAPI, error) {
					return mockClient, nil
				}

				err := b.Setup(ctx, config)
				return b, err
			},
		},
		RollbackPeriod: 1 * time.Second,
	}

	cluster := vault.NewTestCluster(t, coreConfig, &vault.TestClusterOptions{
		HandlerFunc: vaulthttp.Handler,
		NumCores:    2,
	})
	cluster.Start()
	defer cluster.Cleanup()

	cores := cluster.Cores
	vault.TestWaitActive(t, cores[0].Core)
	client := cores[0].Client
	err := client.Sys().Mount("aws", &api.MountInput{
		Type: "aws",
	})
	require.NoError(t, err)

	// create 3 static roles with different rotation periods
	_, err = client.Logical().Write("aws/static-roles/role1", map[string]interface{}{
		"username":        "user1",
		"rotation_period": "2s",
	})
	require.NoError(t, err)
	_, err = client.Logical().Write("aws/static-roles/role2", map[string]interface{}{
		"username":        "user2",
		"rotation_period": "1s",
	})
	require.NoError(t, err)
	_, err = client.Logical().Write("aws/static-roles/role3", map[string]interface{}{
		"username":        "user3",
		"rotation_period": "5m",
	})
	require.NoError(t, err)

	getSecret := func(c *api.Client, role string) string {
		r, err := c.Logical().Read("aws/static-creds/" + role)
		require.NoError(t, err)
		return r.Data["secret_key"].(string)
	}

	role1Secret := getSecret(client, "role1")
	role2Secret := getSecret(client, "role2")
	role3Secret := getSecret(client, "role3")

	verifySecretsRotated := func(c *api.Client, originalRole1Secret, originalRole2Secret, originalRole3Secret string) (updatedRole1Secret, updatedRole2Secret string) {
		testhelpers.RetryUntil(t, 5*time.Second, func() error {
			// verify that both secrets with a low rotation period get rotated
			updatedRole1Secret = getSecret(c, "role1")
			updatedRole2Secret = getSecret(c, "role2")

			if originalRole1Secret == updatedRole1Secret && originalRole2Secret == updatedRole2Secret {
				return fmt.Errorf("secrets haven't been rotated")
			}

			// verify that the secret with a high rotation period doesn't get
			// rotated
			updatedRole3Secret := getSecret(c, "role3")
			if updatedRole3Secret != role3Secret {
				return fmt.Errorf("secret has been rotated but should not have been")
			}
			return nil
		})
		return
	}

	role1Secret, role2Secret = verifySecretsRotated(client, role1Secret, role2Secret, role3Secret)

	// seal to make to core 1 the active node
	cores[0].Seal(t)

	// verify that the correct secrets get rotated again
	vault.TestWaitActive(t, cores[1].Core)
	verifySecretsRotated(cores[1].Client, role1Secret, role2Secret, role3Secret)
}
