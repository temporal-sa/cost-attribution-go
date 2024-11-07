# Cost Attribution (Go)
The purpose of this sample is to demonstrate a basic cost attribution workflow using the Cloud Operations API. It 
assumes that namespaces are used as a unit of isolation for projects.

## What is meant by 'cost attribution'?

Organizations typically have multiple teams using Temporal Cloud. These teams may even make up several isolated business
units with their own budgets, goals, and decision makers. Each of these groups needs to be able to account for their 
usage of Temporal Cloud but in order to keep other concerns manageable (such as SSO or billing) these business units 
typically must operate under one account.

## What does this code do?
This sample is meant to be both inspirational and generally useful. Given isolation of business units by namespace 
(or some way to aggregate them) this sample provides a workflow and activities that will:
1. Pull the usage data for the previous day
2. (Optionally) Store the usage data
3. Calculate the cost associated with the usage for each namespace
4. Store the calculations

## Customize to fit your needs
There are many factors that go into Temporal Cloud pricing and there may be many varied concerns with how costs are 
accounted for within a given organization. As such, there can never be a truly generic cost attribution tool provided by
Temporal. This sample aims to be a starting point for attributing costs using namespaces as a unit of billing
isolation.

This is all powered by the `Storage` and `CostCalculator` interfaces. The implementations given in this sample 
(`InMemoryStorage NaiveCostCalculator`) are useful for a quick demonstration and gut check against your own bill. They 
can be replaced to fit a variety of different needs and for integration with your own applications.

This sample is designed to run as a standalone application so the worker, workflow definition, activity definition, and
workflow execution all run in a single process. In this way all the code is contained in one file and runs via a single
command. This is well-suited for demonstrations and verifying accuracy. However, most cost attributions for production
use cases will want to adapt this sample to run using a 
[Schedule](https://temporal.io/blog/temporal-schedules-reliable-scalable-and-more-flexible-than-cron-jobs) 
and take advantage of existing infrastructure. For example, your organization may already have Temporal workers with the
appropriate secrets in place and therefore additional worker processes would be duplicated effort and compute. For
assistance with designing a solution that fits your environment please request a meeting with a Temporal Cloud Solution 
Architect.


## Prerequisites

To run this sample you will need:
* A Cloud Ops API key w/ permissions to access usage data 
  * (`Global Admin` `Billing Admin`)
    * [create via tcld](https://docs.temporal.io/cloud/tcld/apikey#create) 
    * [create via Cloud UI](https://cloud.temporal.io/settings/api-keys/create)
* A Temporal Cloud namespace to run the worker on:
  * This namespace must use mTLS certificates for auth
      * Modify this sample if you'd like to use a namespace with API auth
  * You'll need the certificates to access this namespace
* Go v1.23.2+
* Temporal Cloud costs per M/Actions, GBHr Active Storage, & GBHr Retained Storage
  * If you do not know these, reach out to your Account Executive
  * More complex accounting (such as accounting for progressive discounts over a month) requires a custom calculator

## Running the sample

Costs per unit given for example, please use your own pricing. 
```bash
export TEMPORAL_CLOUD_OPS_API_KEY=<apiKey>
export TEMPORAL_CLOUD_NAMESPACE_TLS_KEY=</path/to/yourTLSKey.key>
export TEMPORAL_CLOUD_NAMESPACE_TLS_CERT=</path/to/yourTLSCert.pem>
export NAMESPACE=<namespace.accountID>
export COST_PER_MILLION_ACTIONS=25.00
export COST_PER_GBHR_ACTIVE=0.039
export COST_PER_GBHR_RETAINED=0.00042
go run main.go
```

You will see the calculations for each namespace printed in the following format:
```
NAMESPACE: 
TOTAL COST: 
ACTIONS COST:
ACTIVE STORAGE COST:
RETAINED STORAGE COST:
```